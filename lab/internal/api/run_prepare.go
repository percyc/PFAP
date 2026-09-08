package api

import (
	"errors"
	"github.com/pfap/lab/internal/model"
	"math/big"
	"net/http"
	"time"
)

// Preparation is an explicit, separately confirmed action, never part of the
// measured workload. Repeated funding uses a target balance, not an increment.
func (a *API) prepareRunAccounts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExperimentID  string   `json:"experimentId"`
		NodeIDs       []string `json:"nodeIds"`
		Action        string   `json:"action"`
		TargetBalance string   `json:"targetBalance"`
	}
	if err := decode(r, &request); err != nil {
		fail(w, 400, err)
		return
	}
	target, valid := amount(request.TargetBalance)
	if len(request.NodeIDs) == 0 || len(request.NodeIDs) > 300 || (request.Action != "initialize" && request.Action != "fund") || (request.Action == "fund" && (!valid || target.Sign() == 0 || target.BitLen() > 64)) {
		fail(w, 400, errors.New("请选择节点、操作和有效目标余额"))
		return
	}
	var txs []model.Transaction
	batchID := id("prep")
	err := a.store.Update(func(s *model.State) error {
		for _, run := range s.Workloads {
			if run.ExperimentID == request.ExperimentID && runActive(run) {
				return errors.New("运行或收尾期间不能进行账户准备")
			}
		}
		var e model.Experiment
		for _, candidate := range s.Experiments {
			if candidate.ID == request.ExperimentID {
				e = candidate
			}
		}
		if e.Status != "running" {
			return errors.New("实验未运行")
		}
		seen := map[string]bool{}
		for _, nodeID := range request.NodeIDs {
			if seen[nodeID] {
				return errors.New("节点不能重复")
			}
			seen[nodeID] = true
			var n model.Node
			for _, candidate := range e.Nodes {
				if candidate.ID == nodeID {
					n = candidate
				}
			}
			if n.ID == "" || n.Status != "running" || n.PrivateStateError != "" || n.LastSeen.IsZero() || time.Since(n.LastSeen) > 2*time.Minute || transactionNodesBusy(s.Transactions, n.ID, "") {
				return errors.New("所选节点尚未就绪，请刷新状态并等待已有交易完成")
			}
			if !runTradingNode(n) {
				return errors.New("账户准备仅支持已确认不挖矿的普通节点；矿工不参与自动实验交易")
			}
			for _, t := range s.Transactions {
				if t.ExperimentID == e.ID && (t.FromNode == n.ID || t.ToNode == n.ID) && t.ConfirmedAt.After(n.LastSeen) {
					return errors.New("交易结束后的账户状态尚未刷新，请稍后重新检查")
				}
			}
			kind, value := "createAccount", ""
			if request.Action == "initialize" {
				if privateAccountInitialized(s.Transactions, e.ID, n) {
					continue
				}
			} else {
				if !privateStateOnChain(n.LastTxBlock) {
					return errors.New("请先初始化所选账户并等待确认")
				}
				balance, ok := amount(n.ZKBalance)
				public, pubOK := amount(n.PublicBalance)
				if !ok || !pubOK {
					return errors.New("余额采样不可用")
				}
				delta := new(big.Int).Sub(target, balance)
				if delta.Sign() <= 0 {
					continue
				}
				if public.Cmp(delta) <= 0 {
					return errors.New("公开余额不足以 Mint 到目标值并预留费用")
				}
				kind, value = "mint", "0x"+delta.Text(16)
			}
			tx := model.Transaction{ID: id("tx"), BatchID: batchID, ExperimentID: e.ID, Type: kind, FromNode: n.ID, Value: value, Status: "queued", SubmittedAt: time.Now()}
			s.Transactions = append(s.Transactions, tx)
			txs = append(txs, tx)
		}
		return nil
	})
	if err != nil {
		fail(w, 409, err)
		return
	}
	for _, tx := range txs {
		go a.runTransaction(tx)
	}
	jsonOut(w, 202, map[string]any{"queued": len(txs), "batchId": batchID, "action": request.Action})
}
