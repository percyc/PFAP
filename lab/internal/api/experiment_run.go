package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/orchestrator"
)

func amount(s string) (*big.Int, bool) {
	base := 10
	if strings.HasPrefix(s, "0x") {
		base = 16
		s = s[2:]
	}
	n, ok := new(big.Int).SetString(s, base)
	return n, ok && n.Sign() >= 0
}
func runActive(w model.Workload) bool {
	return w.Status == "queued" || w.Status == "running" || w.Status == "draining"
}
func runNodes(e model.Experiment, w model.Workload) []model.Node {
	selected := map[string]bool{}
	for _, id := range w.NodeIDs {
		selected[id] = true
	}
	var nodes []model.Node
	for _, n := range e.Nodes {
		if selected[n.ID] {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// Configured miners remain excluded even when temporarily not mining.
// An unconfigured node actually mining is excluded as well.
func runTradingNode(n model.Node) bool {
	return n.Status == "running" && !n.IsMiner && n.Mining != nil && !*n.Mining
}

func runSampleFresh(n model.Node, now time.Time) bool {
	return !n.LastSeen.IsZero() && now.Sub(n.LastSeen) <= 2*time.Minute
}

func admissionSampleDue(w model.Workload, now time.Time) bool {
	text, _ := w.Configuration["admissionSampleAt"].(string)
	last, err := time.Parse(time.RFC3339Nano, text)
	return err != nil || now.Sub(last) >= 10*time.Second
}

func recordAdmissionSample(s model.State, e model.Experiment, w *model.Workload, now time.Time) {
	if !admissionSampleDue(*w, now) {
		return
	}
	busy, eligible := 0, 0
	stale := []string{}
	for _, n := range runNodes(e, *w) {
		if transactionNodesBusy(s.Transactions, n.ID, "") {
			busy++
			continue
		}
		if !runSampleFresh(n, now) {
			stale = append(stale, n.ID)
			continue
		}
		if runTradingNode(n) && n.StateError == "" && n.PrivateStateError == "" {
			eligible++
		}
	}
	if w.Configuration == nil {
		w.Configuration = map[string]any{}
	}
	samples, _ := w.Configuration["admissionSamples"].([]any)
	samples = append(samples, map[string]any{"at": now.Format(time.RFC3339Nano), "phase": w.Phase, "busyAccounts": busy, "eligibleAccounts": eligible, "staleIdleNodeIds": stale})
	w.Configuration["admissionSamples"] = samples
	w.Configuration["admissionSampleAt"] = now.Format(time.RFC3339Nano)
}

// Preconditions use timestamped monitor data and are checked again at admission.
func runProblems(s model.State, e model.Experiment, w model.Workload, initial bool, now time.Time) []string {
	var problems []string
	if e.Status != "running" {
		problems = append(problems, "实验未运行")
	}
	if e.MiningStatus == "updating" {
		problems = append(problems, "矿工配置正在调整")
	}
	miners := 0
	for _, n := range e.Nodes {
		if n.Status == "running" && n.Mining != nil && *n.Mining {
			miners++
		}
	}
	if miners == 0 {
		problems = append(problems, "尚未观测到在线矿工")
	}
	nodes := runNodes(e, w)
	if len(nodes) < 2 || len(nodes) != len(w.NodeIDs) {
		problems = append(problems, "请选择至少两个不同且属于当前实验的节点")
	}
	value, _ := amount(w.Value)
	for _, n := range nodes {
		reason := ""
		busy := transactionNodesBusy(s.Transactions, n.ID, "")
		switch {
		case n.Status != "running":
			reason = "节点不在线"
		case n.IsMiner || (n.Mining != nil && *n.Mining):
			reason = "矿工专职出块，不能作为自动实验的付款或收款节点"
		case n.Mining == nil:
			reason = "挖矿状态尚未采集，不能确认交易角色"
		case !initial && busy:
			continue // A frozen in-flight commitment is not an idle-account fault.
		case n.PrivateStateError != "" || n.StateError != "":
			reason = "节点或隐私状态异常"
		case n.LastSeen.IsZero() || now.Sub(n.LastSeen) > 2*time.Minute:
			if !initial {
				continue
			} // Quarantine from admission, not a whole-run fault.
			reason = "状态采样过期，请查询节点状态"
		case n.Peers == 0:
			reason = "没有 P2P 连接"
		case !privateStateOnChain(n.LastTxBlock):
			reason = "尚未初始化并确认隐私账户"
		default:
			balance, ok := amount(n.ZKBalance)
			public, pubOK := amount(n.PublicBalance)
			if !ok || value == nil {
				reason = "无法读取隐私余额"
			} else if initial && balance.Cmp(value) < 0 {
				reason = "隐私资金不足，请先 Mint"
			} else if !pubOK || public.Sign() == 0 {
				reason = "公开余额不足或未知，请预留手续费"
			}
		}
		if initial && transactionNodesBusy(s.Transactions, n.ID, "") {
			reason = "存在在途交易或待核验状态"
		}
		if reason != "" {
			problems = append(problems, n.Name+"："+reason)
		}
	}
	return problems
}

func (a *API) createRun(w http.ResponseWriter, r *http.Request, input model.Workload) {
	// Whitelist fields: clients cannot forge phases, timestamps or result counters.
	v := model.Workload{ID: id("load"), ExperimentID: input.ExperimentID, Name: input.Name, Type: "transfer", Value: input.Value, Strategy: "ready-pool", Mode: input.Mode, NodeIDs: input.NodeIDs, WarmupSeconds: input.WarmupSeconds, DurationSeconds: input.DurationSeconds, Confirmations: input.Confirmations, RatePerSecond: input.RatePerSecond, Status: "queued", Phase: "preparing", CreatedAt: time.Now()}
	v.ObserverNodeID = input.ObserverNodeID
	value, ok := amount(v.Value)
	if !ok || value.Sign() == 0 || value.BitLen() > 64 || input.Type != "transfer" || v.DurationSeconds < 1 || v.DurationSeconds > 604800 || v.WarmupSeconds < 1 || v.WarmupSeconds > 7200 || v.Confirmations < 1 || v.Confirmations > 64 || len(v.NodeIDs) < 2 || len(v.NodeIDs) > 300 || (v.Mode != "saturation" && v.Mode != "rate") || math.IsNaN(v.RatePerSecond) || math.IsInf(v.RatePerSecond, 0) || (v.Mode == "rate" && (v.RatePerSecond < 0.01 || v.RatePerSecond > 100)) {
		fail(w, 400, errors.New("请检查 Transfer 数值、节点、负载模式、预热时间、测量时间和确认深度"))
		return
	}
	seen := map[string]bool{}
	for _, id := range v.NodeIDs {
		if seen[id] {
			fail(w, 400, errors.New("节点不能重复"))
			return
		}
		seen[id] = true
	}
	if v.Name == "" {
		v.Name = "Transfer 稳态实验"
	}
	err := a.store.Update(func(s *model.State) error {
		for _, other := range s.Workloads {
			if other.ExperimentID == v.ExperimentID && runActive(other) {
				return errors.New("该实验已有运行或收尾中的自动交易")
			}
		}
		for _, e := range s.Experiments {
			if e.ID == v.ExperimentID {
				var held []*sync.Mutex
				defer func() {
					for _, lock := range held {
						lock.Unlock()
					}
				}()
				for _, n := range e.Nodes {
					lock, _ := a.nodeLocks.LoadOrStore(n.ID, &sync.Mutex{})
					if !lock.(*sync.Mutex).TryLock() {
						return errors.New("节点命令或交易仍在执行，请等待结束")
					}
					held = append(held, lock.(*sync.Mutex))
				}
				if v.ObserverNodeID == "" {
					v.ObserverNodeID = v.NodeIDs[0]
				}
				observerFound := false
				for _, n := range e.Nodes {
					if n.ID == v.ObserverNodeID && n.Status == "running" {
						observerFound = true
					}
				}
				if !observerFound {
					return errors.New("请选择在线的区块观察节点")
				}
				if problems := runProblems(*s, e, v, true, time.Now()); len(problems) > 0 {
					return errors.New(strings.Join(problems, "；"))
				}
				hosts := []map[string]any{}
				for _, server := range s.Servers {
					used := false
					for _, n := range e.Nodes {
						if n.ServerID == server.ID {
							used = true
						}
					}
					if used {
						hosts = append(hosts, map[string]any{"id": server.ID, "host": server.Host, "hostGroup": server.HostGroup, "p2pHost": server.P2PHost, "systemInfo": server.SystemInfo})
					}
				}
				v.Configuration = map[string]any{"networkId": e.NetworkID, "minerCount": e.MinerCount, "minerMode": e.MinerMode, "artifactSha": e.ArtifactSHA, "recoveryArtifactSha": e.RecoveryArtifactSHA, "nodes": e.Nodes, "servers": hosts, "capturedAt": time.Now(), "scheduler": "oldest-idle-payer/lowest-balance-receiver-v1"}
				s.Workloads = append(s.Workloads, v)
				return nil
			}
		}
		return os.ErrNotExist
	})
	if err != nil {
		fail(w, 409, err)
		return
	}
	go a.runExperimentFlow(v)
	jsonOut(w, 202, v)
}

// Longest-idle sender first, lowest-balance eligible receiver next. Selection
// and reservations are committed together, so disjoint transfers may overlap.
func chooseRunPair(s model.State, e model.Experiment, w model.Workload) (model.Node, model.Node, bool) {
	return chooseRunPairAt(s, e, w, time.Now())
}

func chooseRunPairAt(s model.State, e model.Experiment, w model.Workload, now time.Time) (model.Node, model.Node, bool) {
	last := map[string]time.Time{}
	for _, t := range s.Transactions {
		if t.ExperimentID == e.ID {
			for _, id := range []string{t.FromNode, t.ToNode} {
				if t.SubmittedAt.After(last[id]) {
					last[id] = t.SubmittedAt
				}
			}
		}
	}
	var free []model.Node
	for _, n := range runNodes(e, w) {
		if runTradingNode(n) && runSampleFresh(n, now) && n.StateError == "" && n.PrivateStateError == "" && !transactionNodesBusy(s.Transactions, n.ID, "") {
			free = append(free, n)
		}
	}
	sort.SliceStable(free, func(i, j int) bool {
		if last[free[i].ID].Equal(last[free[j].ID]) {
			return free[i].Index < free[j].Index
		}
		return last[free[i].ID].Before(last[free[j].ID])
	})
	value, ok := amount(w.Value)
	if !ok {
		return model.Node{}, model.Node{}, false
	}
	for _, payer := range free {
		balance, ok := amount(payer.ZKBalance)
		if !ok || balance.Cmp(value) < 0 {
			continue
		}
		var receivers []model.Node
		for _, receiver := range free {
			b, ok := amount(receiver.ZKBalance)
			if receiver.ID != payer.ID && ok && new(big.Int).Add(b, value).BitLen() <= 64 {
				receivers = append(receivers, receiver)
			}
		}
		sort.SliceStable(receivers, func(i, j int) bool {
			x, _ := amount(receivers[i].ZKBalance)
			y, _ := amount(receivers[j].ZKBalance)
			return x.Cmp(y) < 0
		})
		if len(receivers) > 0 {
			return payer, receivers[0], true
		}
	}
	return model.Node{}, model.Node{}, false
}

// One bounded scheduling step. Wall-clock deadlines are checked inside the
// durable admission boundary; a delayed ticker cannot submit beyond the window.
func (a *API) runFlowTick(id string, now time.Time) (*model.Transaction, bool, error) {
	// Saturation polls are not offered requests. Avoid rewriting the entire
	// JSON store while every account is occupied; always recheck atomically
	// before an actual admission. Phase/fault transitions still take the write path.
	idlePoll := false
	a.store.View(func(s model.State) {
		for _, w := range s.Workloads {
			if w.ID != id || w.Mode != "saturation" || w.Status != "running" || w.StopRequested || w.BlockError != "" {
				continue
			}
			if w.Phase == "warmup" && now.Sub(w.StartedAt) >= time.Duration(w.WarmupSeconds)*time.Second {
				continue
			}
			if w.Phase == "measuring" && !now.Before(w.MeasurementEndsAt) {
				continue
			}
			var e model.Experiment
			for _, x := range s.Experiments {
				if x.ID == w.ExperimentID {
					e = x
				}
			}
			if len(runProblems(s, e, w, false, now)) > 0 {
				continue
			}
			fault := false
			for _, t := range s.Transactions {
				if t.WorkloadID == id && (t.Status == "unknown" || t.Status == "failed" || t.Status == "timeout" || (t.Status == "settling" && t.Error != "")) {
					fault = true
				}
			}
			if !fault {
				_, _, ok := chooseRunPairAt(s, e, w, now)
				idlePoll = !ok && !admissionSampleDue(w, now)
			}
		}
	})
	if idlePoll {
		return nil, false, nil
	}
	var queued *model.Transaction
	done := false
	err := a.store.Update(func(s *model.State) error {
		for wi := range s.Workloads {
			w := &s.Workloads[wi]
			if w.ID != id {
				continue
			}
			if w.Status != "running" || w.StopRequested {
				done = true
				return nil
			}
			var e model.Experiment
			for _, candidate := range s.Experiments {
				if candidate.ID == w.ExperimentID {
					e = candidate
				}
			}
			fault := ""
			recordAdmissionSample(*s, e, w, now)
			if w.BlockError != "" {
				fault = w.BlockError
			}
			for _, t := range s.Transactions {
				if t.WorkloadID == id && (t.Status == "unknown" || t.Status == "failed" || t.Status == "timeout" || (t.Status == "settling" && t.Error != "")) {
					fault = "交易或双方状态核验异常，已停止新增投递"
				}
			}
			if problems := runProblems(*s, e, *w, false, now); len(problems) > 0 {
				fault = strings.Join(problems, "；")
			}
			if fault != "" {
				w.InvalidReason = fault
				w.StopRequested = true
				w.Status = "draining"
				w.Phase = "draining"
				w.SubmissionStoppedAt = now
				done = true
				return nil
			}
			if w.Phase == "warmup" && now.Sub(w.StartedAt) >= time.Duration(w.WarmupSeconds)*time.Second {
				participated := map[string]bool{}
				for _, t := range s.Transactions {
					if t.WorkloadID == id && t.Status == "confirmed" && !t.ReadyAt.IsZero() && !t.ReadyAt.Before(runBlockWarmupStart(*w)) {
						participated[t.FromNode] = true
						participated[t.ToNode] = true
					}
				}
				ready := len(w.Blocks) > 1 && runBlockWarmupReady(*w, now)
				for _, node := range w.NodeIDs {
					if !participated[node] {
						ready = false
					}
				}
				if ready {
					w.Phase = "measuring"
					w.MeasurementStartedAt = now
					w.MeasurementEndsAt = now.Add(time.Duration(w.DurationSeconds) * time.Second)
				} else if now.Sub(w.StartedAt) > time.Duration(w.WarmupSeconds+1800)*time.Second {
					w.InvalidReason = "预热超时：不是所有参与账户都完成了安全交易"
					w.Status = "draining"
					w.Phase = "draining"
					w.SubmissionStoppedAt = now
					done = true
					return nil
				}
			}
			if w.Phase == "measuring" && !now.Before(w.MeasurementEndsAt) {
				w.Status = "draining"
				w.Phase = "draining"
				w.SubmissionStoppedAt = w.MeasurementEndsAt
				done = true
				return nil
			}
			w.Attempted++
			payer, receiver, ok := chooseRunPairAt(*s, e, *w, now)
			if !ok {
				w.SkippedBusy++
				return nil
			}
			p, _ := amount(payer.ZKBalance)
			q, _ := amount(receiver.ZKBalance)
			value, _ := amount(w.Value)
			tx := model.Transaction{ID: newRunTxID(), WorkloadID: id, Sequence: w.Submitted + 1, ExperimentID: w.ExperimentID, Type: "transfer", FromNode: payer.ID, ToNode: receiver.ID, Value: w.Value, Status: "queued", RunPhase: w.Phase, SubmittedAt: now, ExpectedPayerBalance: new(big.Int).Sub(p, value).String(), ExpectedReceiverBalance: new(big.Int).Add(q, value).String()}
			s.Transactions = append(s.Transactions, tx)
			w.Submitted++
			queued = &tx
			return nil
		}
		done = true
		return os.ErrNotExist
	})
	if err != nil {
		return nil, true, err
	}
	return queued, done, nil
}
func newRunTxID() string { return id("tx") }
func (a *API) runExperimentFlow(v model.Workload) {
	if err := a.store.Update(func(s *model.State) error {
		for i := range s.Workloads {
			w := &s.Workloads[i]
			if w.ID == v.ID {
				if w.Status != "queued" || w.StopRequested {
					return errWorkloadStopped
				}
				w.Status = "running"
				w.Phase = "warmup"
				w.StartedAt = time.Now()
				return nil
			}
		}
		return os.ErrNotExist
	}); err != nil {
		return
	}
	interval := 100 * time.Millisecond
	if v.Mode == "rate" {
		interval = time.Duration(float64(time.Second) / v.RatePerSecond)
	}
	clockInterval := 100 * time.Millisecond
	if interval < clockInterval {
		clockInterval = interval
	}
	ticker := time.NewTicker(clockInterval)
	defer ticker.Stop()
	blockStop := make(chan struct{})
	defer close(blockStop)
	go a.collectRunBlocks(v.ID, blockStop)
	next := time.Now()
	for now := range ticker.C {
		// Stop remains responsive even at 0.01 attempts/s.
		stopped := false
		windowEnded := false
		a.store.View(func(s model.State) {
			for _, w := range s.Workloads {
				if w.ID == v.ID {
					stopped = w.StopRequested || w.Status != "running"
					windowEnded = w.Phase == "measuring" && !now.Before(w.MeasurementEndsAt)
				}
			}
		})
		if stopped {
			a.drainWorkload(v, 0)
			return
		}
		if now.Before(next) && !windowEnded {
			continue
		}
		next = now.Add(interval)
		burst := 1
		if v.Mode == "saturation" {
			burst = len(v.NodeIDs) / 2
		}
		for i := 0; i < burst; i++ {
			tx, done, err := a.runFlowTick(v.ID, time.Now())
			if err != nil {
				a.finishWorkload(v.ID, "interrupted", err.Error())
				return
			}
			if done {
				a.drainWorkload(v, 0)
				return
			}
			if tx == nil {
				break
			}
			go a.runTransaction(*tx)
		}
	}
}

type readinessSample struct {
	TransactionHash string `json:"transactionHash"`
	Hash            string `json:"hash"`
	Canonical       string `json:"canonical"`
	Head            uint64 `json:"head"`
	Block           uint64 `json:"block"`
	Status          string `json:"status"`
	Balance         string `json:"balance"`
	StateBlock      string `json:"stateBlock"`
	CommitmentReady bool   `json:"commitmentReady"`
}

func validRunReadiness(x readinessSample, hash, expected string, confirmations int) bool {
	b, ok := amount(x.Balance)
	want, valid := amount(expected)
	stateBlock, validBlock := parseBlockValue(x.StateBlock)
	return ok && valid && validBlock && b.Cmp(want) == 0 && x.CommitmentReady && x.Hash != "" && strings.EqualFold(x.Hash, x.Canonical) && (x.Status == "0x1" || x.Status == "1") && x.Block > 0 && stateBlock == x.Block && x.Head >= x.Block && x.Head-x.Block+1 >= uint64(confirmations) && hash != "" && strings.EqualFold(x.TransactionHash, hash)
}

// Called while both account execution locks are held. Never replays an RPC.
func (a *API) checkRunReadiness(ctx context.Context, txID string) error {
	var tx model.Transaction
	var e model.Experiment
	var w model.Workload
	servers := map[string]model.Server{}
	a.store.View(func(s model.State) {
		for _, t := range s.Transactions {
			if t.ID == txID {
				tx = t
			}
		}
		for _, x := range s.Experiments {
			if x.ID == tx.ExperimentID {
				e = x
			}
		}
		for _, x := range s.Workloads {
			if x.ID == tx.WorkloadID {
				w = x
			}
		}
		for _, x := range s.Servers {
			servers[x.ID] = x
		}
	})
	if tx.RunPhase == "" || tx.Status != "settling" {
		return nil
	}
	expr := `(function(){var r=eth.getTransactionReceipt(` + strconv.Quote(tx.Hash) + `),b=r?eth.getBlock(r.blockNumber):null,z=eth.getAccountState();return JSON.stringify({transactionHash:r?r.transactionHash:"",hash:r?r.blockHash:"",canonical:b?b.hash:"",head:eth.blockNumber,block:r?r.blockNumber:0,status:r?String(r.status):"",balance:z.balance,stateBlock:z.lastTxBlockNumber,commitmentReady:z.commitmentReady})})()`
	lastErr := errors.New("等待双方规范链确认、承诺和余额核验")
	for ctx.Err() == nil {
		hashes := []string{}
		ready := true
		for _, nodeID := range []string{tx.FromNode, tx.ToNode} {
			var n model.Node
			for _, x := range e.Nodes {
				if x.ID == nodeID {
					n = x
				}
			}
			out, err := a.orch.Attach(ctx, e, n, servers[n.ServerID], expr)
			if err != nil {
				ready = false
				break
			}
			raw, err := orchestrator.ExtractJSONString(out)
			if err != nil {
				ready = false
				break
			}
			var sample readinessSample
			expected := tx.ExpectedPayerBalance
			if nodeID == tx.ToNode {
				expected = tx.ExpectedReceiverBalance
			}
			if json.Unmarshal([]byte(raw), &sample) != nil || !validRunReadiness(sample, tx.Hash, expected, w.Confirmations) {
				ready = false
				break
			}
			hashes = append(hashes, sample.Hash)
			if err := a.sampleNode(ctx, e, n, servers[n.ServerID], "run-readiness:"+tx.ID); err != nil {
				ready = false
				break
			}
		}
		if ready && len(hashes) == 2 && strings.EqualFold(hashes[0], hashes[1]) {
			return a.store.Update(func(s *model.State) error {
				for i := range s.Transactions {
					t := &s.Transactions[i]
					if t.ID == txID && t.Status == "settling" {
						t.Status = "confirmed"
						t.ReadyAt = time.Now()
						t.Error = ""
					}
				}
				return nil
			})
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Transactions {
			if s.Transactions[i].ID == txID && s.Transactions[i].Status == "settling" {
				s.Transactions[i].Error = lastErr.Error() + "；未释放双方，请核验。"
			}
		}
		return nil
	})
	return lastErr
}

// Window report uses a fixed denominator, and retains right-censored submissions.
func runReport(s model.State, w model.Workload, now time.Time) map[string]any {
	total, confirmed, pending, failed, windowConfirmed := 0, 0, 0, 0, 0
	var latency []int64
	var chainLatency, readyLatency []int64
	minuteCounts := map[int]int{}
	end := w.MeasurementEndsAt
	if !w.SubmissionStoppedAt.IsZero() && w.SubmissionStoppedAt.Before(end) {
		end = w.SubmissionStoppedAt
	}
	if end.After(now) {
		end = now
	}
	seconds := 0.0
	if !w.MeasurementStartedAt.IsZero() {
		seconds = math.Max(0, end.Sub(w.MeasurementStartedAt).Seconds())
	}
	for _, t := range s.Transactions {
		if t.WorkloadID != w.ID {
			continue
		}
		if !w.MeasurementStartedAt.IsZero() && t.Status == "confirmed" && !t.ConfirmedAt.Before(w.MeasurementStartedAt) && t.ConfirmedAt.Before(end) {
			windowConfirmed++
			minuteCounts[int(t.ConfirmedAt.Sub(w.MeasurementStartedAt).Minutes())]++
		}
		if t.RunPhase != "measuring" {
			continue
		}
		total++
		switch t.Status {
		case "confirmed":
			confirmed++
			latency = append(latency, t.ConfirmedAt.Sub(t.SubmittedAt).Milliseconds())
			if !t.BroadcastAt.IsZero() {
				chainLatency = append(chainLatency, t.ConfirmedAt.Sub(t.BroadcastAt).Milliseconds())
			}
			if !t.ReadyAt.IsZero() {
				readyLatency = append(readyLatency, t.ReadyAt.Sub(t.SubmittedAt).Milliseconds())
			}
		case "failed", "timeout", "cancelled":
			failed++
		default:
			pending++
		}
	}
	sum := int64(0)
	for _, v := range latency {
		sum += v
	}
	var mean any
	var tps any
	if len(latency) > 0 {
		mean = float64(sum) / float64(len(latency))
	}
	if seconds > 0 {
		tps = float64(windowConfirmed) / seconds
	}
	minuteSeries := []map[string]any{}
	for m := 0; m < int(math.Ceil(seconds/60)); m++ {
		span := math.Min(60, seconds-float64(m*60))
		minuteSeries = append(minuteSeries, map[string]any{"minute": m + 1, "seconds": span, "confirmed": minuteCounts[m], "observedTPS": float64(minuteCounts[m]) / span})
	}
	blocks := blockRunReport(w, end)
	return map[string]any{"workload": w, "minuteSeries": minuteSeries, "chainLatencyP95Ms": percentile(chainLatency, .95), "readyCycleP95Ms": percentile(readyLatency, .95), "blocks": blocks, "windowSeconds": seconds, "measurementSubmissions": total, "confirmedSubmissions": confirmed, "pendingSubmissions": pending, "failedSubmissions": failed, "windowConfirmed": windowConfirmed, "observedConfirmedTPS": tps, "meanLatencyMs": mean, "p95LatencyMs": percentile(latency, .95), "p99LatencyMs": percentile(latency, .99), "valid": w.Status == "completed" && w.InvalidReason == "" && w.BlockError == "" && blocks["count"].(int) > 1 && !w.StopRequested && !w.MeasurementStartedAt.IsZero() && pending == 0 && failed == 0 && confirmed > 0, "limitations": []string{"吞吐按控制器观察到 Receipt 并完成日志查询后的记录时间计数，不是精确链上打包时间；包含轮询与采集误差。", "预热完成仅代表所有参与账户已完成安全交易，不自动宣称统计平稳。", "区块为单观察节点采样口径；执行与状态验证耗时需要新版 geth 埋点，缺失时为 null，不能称作完整区块验证耗时。", "确认策略为 PoW 深度确认，不保证最终性；账户就绪核验不等同于 SN 文件持久化证明。"}}
}

func (a *API) runReadAPI(w http.ResponseWriter, r *http.Request, runID string) {
	if r.URL.Query().Get("format") != "" {
		a.exportResults(w, r, "", runID)
		return
	}
	found := false
	a.store.View(func(s model.State) {
		for _, run := range s.Workloads {
			if run.ID == runID {
				found = true
				result := runReport(s, run, time.Now())
				result["workload"] = safeRun(run)
				jsonOut(w, 200, result)
				return
			}
		}
	})
	if !found {
		fail(w, 404, os.ErrNotExist)
	}
}
