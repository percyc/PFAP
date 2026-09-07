package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/orchestrator"
)

var transactionHashPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)

type uncertainResult struct {
	hash  string
	cause error
}

type transactionFinish struct {
	status string
	hash   string
	cause  error
}

func (a *API) flushPendingTransactionResults() {
	a.pendingUncertainty.Range(func(key, value any) bool {
		result := value.(uncertainResult)
		_ = a.markTransactionUnknown(key.(string), result.hash, result.cause)
		return true
	})
	a.pendingTxFinishes.Range(func(key, value any) bool {
		result := value.(transactionFinish)
		_ = a.finishTx(key.(string), result.status, result.hash, result.cause)
		return true
	})
}

// Restart never replays an RPC. Only work which had not started is cancelled;
// potentially executed calls retain their node reservation until reconciled.
func reconcileInterruptedTasks(s *model.State) {
	now := time.Now()
	for i := range s.Experiments {
		e := &s.Experiments[i]
		if e.Status == "running" {
			for j := range e.Nodes {
				if e.Nodes[j].Status == "running" {
					e.Nodes[j].Status, e.Nodes[j].Mining = "unknown", nil
					e.Nodes[j].StateError = "控制器已重启，等待首次节点状态检查。"
				}
			}
		}
		if e.Status == "deploying" || e.Status == "resuming" || e.Status == "stopping" {
			e.Status = "interrupted"
			e.Error = "控制器重启中断了操作，节点实际状态尚未确认；可按原运行包继续或重新检查停止。"
			for j := range e.Nodes {
				e.Nodes[j].Status, e.Nodes[j].Mining = "unknown", nil
			}
		}
	}
	for i := range s.Transactions {
		tx := &s.Transactions[i]
		switch {
		case tx.Status == "queued" && tx.ProvingAt.IsZero() && tx.Hash == "" && tx.SubmissionAttemptedAt.IsZero():
			tx.Status, tx.Error = "cancelled", "控制器重启，尚未开始的交易已取消，未自动重发。"
		case tx.Status == "proving" || tx.Status == "submitted" || tx.Status == "queued" || (tx.Receipt == "" && tx.Hash != "" && (tx.Status == "timeout" || tx.Status == "failed")):
			tx.Status, tx.Error = "unknown", "执行或确认被中断，结果待核验；不会自动重发，相关节点继续保留交易占用。"
			tx.ConfirmedAt = time.Time{}
		}
	}
	for i := range s.Workloads {
		w := &s.Workloads[i]
		if w.Status == "queued" || w.Status == "running" || w.Status == "draining" {
			w.Status, w.StopRequested, w.FinishedAt = "interrupted", true, now
			if w.SubmissionStoppedAt.IsZero() {
				w.SubmissionStoppedAt = now
			}
			w.Error = "控制器重启，已停止新增投递；已发送交易单独核验，不自动续投。"
		}
	}
	for i := range s.Servers {
		srv := &s.Servers[i]
		if srv.Status == "error" && srv.LastError == "" && !strings.Contains(srv.SystemInfo, "host=") {
			srv.LastError, srv.SystemInfo = srv.SystemInfo, ""
		} else if srv.Status == "online" && srv.LastSuccessAt.IsZero() {
			srv.LastSuccessAt = srv.LastCheck
		}
	}
}

func (a *API) recordServerCheck(serverID, output string, checkErr error) error {
	return a.store.Update(func(s *model.State) error {
		for i := range s.Servers {
			srv := &s.Servers[i]
			if srv.ID != serverID {
				continue
			}
			srv.LastCheck = time.Now()
			if checkErr != nil {
				srv.Status, srv.LastError = "error", checkErr.Error()
			} else {
				srv.Status, srv.LastError, srv.SystemInfo = "online", "", output
				srv.LastSuccessAt = srv.LastCheck
			}
			return nil
		}
		return os.ErrNotExist
	})
}

// This is the durable boundary before an RPC may change node state. If the
// intent cannot be saved, the RPC must not run.
func (a *API) beginTransactionRPC(txID, stage string) error {
	return a.store.Update(func(s *model.State) error {
		for i := range s.Transactions {
			tx := &s.Transactions[i]
			if tx.ID != txID {
				continue
			}
			if tx.Status != "queued" && tx.Status != "proving" {
				return errors.New("交易已停止执行或结果待核验，未发送新指令")
			}
			if err := transactionNodesAvailable(*s, *tx); err != nil {
				return err
			}
			tx.Status, tx.ExecutionStage = "proving", stage
			if tx.ProvingAt.IsZero() {
				tx.ProvingAt = time.Now()
			}
			if stage == "submit" {
				tx.SubmissionAttemptedAt = time.Now()
			}
			return nil
		}
		return os.ErrNotExist
	})
}

func (a *API) markTransactionUnknown(txID, hash string, cause error) error {
	message := "执行结果待核验；不会自动重发，相关节点继续保留交易占用。"
	if cause != nil {
		message += " " + cause.Error()
	}
	err := a.store.Update(func(s *model.State) error {
		for i := range s.Transactions {
			tx := &s.Transactions[i]
			if tx.ID != txID {
				continue
			}
			if tx.Receipt != "" || tx.Status == "confirmed" {
				return nil
			}
			tx.Status, tx.Error, tx.ConfirmedAt = "unknown", message, time.Time{}
			if hash != "" {
				tx.Hash = hash
			}
			return nil
		}
		return os.ErrNotExist
	})
	if err != nil {
		// Retain evidence in memory while storage is unavailable. The durable
		// pre-RPC intent still protects against replay after a controller crash.
		a.pendingUncertainty.Store(txID, uncertainResult{hash: hash, cause: cause})
	} else {
		a.pendingUncertainty.Delete(txID)
	}
	return err
}

func (a *API) monitorUncertainTransactions() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		a.flushPendingTransactionResults()
		var ids []string
		a.store.View(func(s model.State) {
			for _, tx := range s.Transactions {
				if tx.Status == "unknown" && transactionHashPattern.MatchString(tx.Hash) {
					ids = append(ids, tx.ID)
				}
			}
		})
		// Bounded read-only polling. No transaction submission or state rollback.
		for _, txID := range ids {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, _ = a.reconcileTransaction(ctx, txID)
			cancel()
		}
		<-ticker.C
	}
}

func (a *API) reconcileTransaction(ctx context.Context, txID string) (model.Transaction, error) {
	lockAny, _ := a.reconcileLocks.LoadOrStore(txID, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	if !lock.TryLock() {
		return model.Transaction{}, errors.New("该交易正在核验，请稍候")
	}
	defer lock.Unlock()
	var tx model.Transaction
	var exp model.Experiment
	servers := map[string]model.Server{}
	a.store.View(func(s model.State) {
		for _, t := range s.Transactions {
			if t.ID == txID {
				tx = t
			}
		}
		for _, e := range s.Experiments {
			if e.ID == tx.ExperimentID {
				exp = e
			}
		}
		for _, srv := range s.Servers {
			servers[srv.ID] = srv
		}
	})
	if tx.ID == "" {
		return tx, os.ErrNotExist
	}
	if tx.Status == "confirmed" || tx.Receipt != "" {
		return tx, nil
	}
	if tx.Status != "unknown" && tx.Status != "submitted" && tx.Status != "timeout" && !(tx.Status == "failed" && tx.Hash != "") {
		return tx, errors.New("该交易尚未提交或无需核验")
	}
	finishCheck := func(checkErr error) (model.Transaction, error) {
		err := a.store.Update(func(s *model.State) error {
			for i := range s.Transactions {
				current := &s.Transactions[i]
				if current.ID != txID {
					continue
				}
				current.LastCheckedAt = time.Now()
				current.ReconciliationError = ""
				if checkErr != nil {
					current.ReconciliationError = checkErr.Error()
				}
				tx = *current
			}
			return nil
		})
		return tx, errors.Join(checkErr, err)
	}
	if !transactionHashPattern.MatchString(tx.Hash) {
		return finishCheck(errors.New("没有可核验的交易哈希，无法断言交易未发送；请人工检查节点日志和双方账户状态，勿重复发送或初始化"))
	}
	lockIDs := []string{tx.FromNode}
	if tx.Type == "transfer" && tx.ToNode != "" && tx.ToNode != tx.FromNode {
		lockIDs = append(lockIDs, tx.ToNode)
	}
	sort.Strings(lockIDs)
	var held []*sync.Mutex
	defer func() {
		for _, l := range held {
			l.Unlock()
		}
	}()
	for _, nodeID := range lockIDs {
		l, _ := a.nodeLocks.LoadOrStore(nodeID, &sync.Mutex{})
		if !l.(*sync.Mutex).TryLock() {
			return tx, errors.New("交易执行或原确认查询尚未结束，请稍后核验")
		}
		held = append(held, l.(*sync.Mutex))
	}
	var node model.Node
	queryNodeID := tx.FromNode
	if tx.Type == "transfer" {
		queryNodeID = tx.ToNode
	}
	for _, n := range exp.Nodes {
		if n.ID == queryNodeID {
			node = n
		}
	}
	server, exists := servers[node.ServerID]
	if !exists || node.ID == "" || exp.Status != "running" || node.Status != "running" {
		return finishCheck(errors.New("查询节点尚未恢复运行，请先继续原实验或恢复节点；原交易保持待核验"))
	}
	expr := `(function(){var r=eth.getTransactionReceipt(` + strconv.Quote(tx.Hash) + `);var b=r&&r.blockNumber!=null?eth.getBlock(r.blockNumber):null;return JSON.stringify({receipt:r,canonicalHash:b?b.hash:null})})()`
	out, err := a.orch.Attach(ctx, exp, node, server, expr)
	if err != nil {
		return finishCheck(fmt.Errorf("Receipt 查询失败：%w", err))
	}
	raw, err := orchestrator.ExtractJSONString(out)
	if err != nil {
		return finishCheck(err)
	}
	var result struct {
		Receipt       json.RawMessage `json:"receipt"`
		CanonicalHash string          `json:"canonicalHash"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return finishCheck(err)
	}
	if len(result.Receipt) == 0 || string(result.Receipt) == "null" {
		return finishCheck(errors.New("尚未查到链上 Receipt，不能据此判定失败或重新发送"))
	}
	var receipt struct {
		Hash        string `json:"transactionHash"`
		BlockHash   string `json:"blockHash"`
		BlockNumber any    `json:"blockNumber"`
		Status      any    `json:"status"`
	}
	if err := json.Unmarshal(result.Receipt, &receipt); err != nil {
		return finishCheck(err)
	}
	status := fmt.Sprint(receipt.Status)
	block := fmt.Sprint(receipt.BlockNumber)
	_, blockOK := parseBlockValue(block)
	if !strings.EqualFold(receipt.Hash, tx.Hash) || receipt.BlockHash == "" || !strings.EqualFold(result.CanonicalHash, receipt.BlockHash) || !blockOK || (status != "0x1" && status != "1" && status != "0x0" && status != "0") {
		return finishCheck(errors.New("Receipt 的哈希、主链区块或执行状态无法确认，保留待核验"))
	}
	if err := a.confirmTx(tx.ID, tx.Hash, string(result.Receipt), block, status, tx.ProofDurationUs, tx.VerifyDurationUs); err != nil {
		return finishCheck(err)
	}
	// Refresh is read-only; it never initializes or replays the account.
	var from, to model.Node
	for _, n := range exp.Nodes {
		if n.ID == tx.FromNode {
			from = n
		}
		if n.ID == tx.ToNode {
			to = n
		}
	}
	a.refreshTransactionNodesContext(ctx, exp, from, to, servers[from.ServerID], tx.ID)
	return finishCheck(nil)
}

func (a *API) transactionAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[3] != "reconcile" {
		fail(w, 404, errors.New("unknown transaction action"))
		return
	}
	if r.Method != http.MethodPost {
		fail(w, 405, errors.New("method not allowed"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tx, err := a.reconcileTransaction(ctx, parts[2])
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, os.ErrNotExist) {
			code = http.StatusNotFound
		}
		fail(w, code, err)
		return
	}
	jsonOut(w, http.StatusOK, tx)
}
