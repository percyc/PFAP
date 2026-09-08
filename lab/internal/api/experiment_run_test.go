package api

import (
	"bytes"
	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/store"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunMinerRolesRejected(t *testing.T) {
	for _, role := range []string{"configured", "actual", "unknown"} {
		t.Run(role, func(t *testing.T) {
			a, now := runFixture(t)
			saveTestState(t, a, func(s *model.State) {
				n := &s.Experiments[0].Nodes[0]
				switch role {
				case "configured":
					n.IsMiner = true
				case "actual":
					mining := true
					n.Mining = &mining
				case "unknown":
					n.Mining = nil
				}
			})
			a.store.View(func(s model.State) {
				if runTradingNode(s.Experiments[0].Nodes[0]) {
					t.Fatal("unsafe role admitted")
				}
				if len(runProblems(s, s.Experiments[0], s.Workloads[0], true, now)) == 0 {
					t.Fatal("preflight accepted unsafe role")
				}
			})
			tx, _, err := a.runFlowTick("w", now)
			if err != nil || tx != nil {
				t.Fatalf("role change admitted transaction: %v", err)
			}
		})
	}
}

func TestRunPreparationRejectsMinerAtomically(t *testing.T) {
	a, _ := runFixture(t)
	saveTestState(t, a, func(s *model.State) { s.Workloads = nil; s.Experiments[0].Nodes[1].IsMiner = true })
	rr := httptest.NewRecorder()
	a.prepareRunAccounts(rr, httptest.NewRequest("POST", "/api/workloads/prepare", bytes.NewBufferString(`{"experimentId":"e","nodeIds":["a","b"],"action":"fund","targetBalance":"20"}`)))
	if rr.Code != 409 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	a.store.View(func(s model.State) {
		if len(s.Transactions) != 0 {
			t.Fatal("partial batch committed")
		}
	})
}

func TestRunPreparationIsIdempotentAndAtomic(t *testing.T) {
	a, _ := runFixture(t)
	saveTestState(t, a, func(s *model.State) { s.Workloads = nil })
	rr := httptest.NewRecorder()
	a.prepareRunAccounts(rr, httptest.NewRequest("POST", "/api/workloads/prepare", bytes.NewBufferString(`{"experimentId":"e","nodeIds":["a","b"],"action":"fund","targetBalance":"10"}`)))
	if rr.Code != 202 || !strings.Contains(rr.Body.String(), `"queued":0`) {
		t.Fatal(rr.Body.String())
	}
	rr = httptest.NewRecorder()
	a.prepareRunAccounts(rr, httptest.NewRequest("POST", "/api/workloads/prepare", bytes.NewBufferString(`{"experimentId":"e","nodeIds":["a","missing"],"action":"fund","targetBalance":"20"}`)))
	if rr.Code != 409 {
		t.Fatal(rr.Body.String())
	}
	a.store.View(func(s model.State) {
		if len(s.Transactions) != 0 {
			t.Fatal("partial batch committed")
		}
	})
}
func TestRunBlockReportKeepsMissingSamplesMissing(t *testing.T) {
	start := time.Now()
	v := int64(50)
	w := model.Workload{MeasurementStartedAt: start, Blocks: []model.RunBlock{{Timestamp: 1, Size: 100, ObservedAt: start.Add(-time.Second)}, {Timestamp: 5, Size: 200, Transactions: 1, ObservedAt: start.Add(time.Second), ValidationUs: &v}, {Timestamp: 15, Size: 100, ObservedAt: start.Add(2 * time.Second)}}}
	r := blockRunReport(w, start.Add(time.Hour))
	if r["count"] != 2 || r["meanSizeB"] != float64(150) || r["meanBlockIntervalSeconds"] != float64(10) || r["missingValidationSamples"] != 1 || r["meanExecutionValidationUs"] != float64(50) {
		t.Fatal(r)
	}
}

func runFixture(t *testing.T) (*API, time.Time) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	a := &API{store: db}
	now := time.Now()
	mining := false
	s := model.State{Experiments: []model.Experiment{{ID: "e", Status: "running", Nodes: []model.Node{}}}, Workloads: []model.Workload{{ID: "w", ExperimentID: "e", Strategy: "ready-pool", Type: "transfer", Value: "0x1", Status: "running", Phase: "warmup", Mode: "saturation", WarmupSeconds: 10, DurationSeconds: 3600, StartedAt: now, Confirmations: 2, NodeIDs: []string{"a", "b", "c", "d"}}}}
	for i, id := range []string{"a", "b", "c", "d"} {
		s.Experiments[0].Nodes = append(s.Experiments[0].Nodes, model.Node{ID: id, Name: id, Index: i + 1, Status: "running", ZKBalance: "0xa", PublicBalance: "1000", LastSeen: now, LastTxBlock: "0x1", Peers: 3, Mining: &mining})
	}
	activeMining := true
	s.Experiments[0].Nodes = append(s.Experiments[0].Nodes, model.Node{ID: "miner", Name: "miner", Index: 5, Status: "running", IsMiner: true, Mining: &activeMining})
	saveTestState(t, a, func(x *model.State) { *x = s })
	return a, now
}
func TestRunAtomicDisjointReservations(t *testing.T) {
	a, now := runFixture(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var txs []*model.Transaction
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, _, err := a.runFlowTick("w", now)
			if err != nil {
				t.Error(err)
			}
			if tx != nil {
				mu.Lock()
				txs = append(txs, tx)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(txs) != 2 {
		t.Fatalf("queued %d, expected 2", len(txs))
	}
	seen := map[string]bool{}
	for _, tx := range txs {
		for _, id := range []string{tx.FromNode, tx.ToNode} {
			if seen[id] {
				t.Fatal("duplicate account reservation")
			}
			seen[id] = true
		}
		if tx.ExpectedPayerBalance != "9" || tx.ExpectedReceiverBalance != "11" {
			t.Fatal("balance expectation not fixed at admission")
		}
	}
}
func TestRunBoundaryAndWarmupGate(t *testing.T) {
	a, now := runFixture(t)
	saveTestState(t, a, func(s *model.State) {
		s.Workloads[0].Blocks = []model.RunBlock{{Number: 1}, {Number: 2}}
		s.Transactions = []model.Transaction{{WorkloadID: "w", Status: "confirmed", ReadyAt: now, FromNode: "a", ToNode: "b"}, {WorkloadID: "w", Status: "confirmed", ReadyAt: now, FromNode: "c", ToNode: "d"}}
	})
	tx, done, err := a.runFlowTick("w", now.Add(11*time.Second))
	if err != nil || done || tx.RunPhase != "measuring" {
		t.Fatalf("window did not start: %v", err)
	}
	saveTestState(t, a, func(s *model.State) {
		for i := range s.Experiments[0].Nodes {
			s.Experiments[0].Nodes[i].LastSeen = now.Add(3611 * time.Second)
		}
	})
	tx, done, err = a.runFlowTick("w", now.Add(3611*time.Second))
	if err != nil || !done || tx != nil {
		t.Fatal("boundary admitted a transaction")
	}
	a.store.View(func(s model.State) {
		if s.Workloads[0].SubmissionStoppedAt != s.Workloads[0].MeasurementEndsAt {
			t.Fatal("window denominator extended")
		}
	})
}
func TestRunStopFaultAndManualIsolation(t *testing.T) {
	a, now := runFixture(t)
	if a.enqueueTransaction(model.Transaction{ExperimentID: "e", FromNode: "a", Type: "mint"}) == nil {
		t.Fatal("manual injection allowed")
	}
	saveTestState(t, a, func(s *model.State) {
		s.Transactions = append(s.Transactions, model.Transaction{ID: "uncertain", WorkloadID: "w", Status: "unknown", FromNode: "a", ToNode: "b"})
	})
	tx, done, err := a.runFlowTick("w", now)
	if err != nil || !done || tx != nil {
		t.Fatal("fault did not stop admission")
	}
	a.store.View(func(s model.State) {
		if s.Workloads[0].InvalidReason == "" || !transactionNodesBusy(s.Transactions, "a", "") {
			t.Fatal("fault evidence or account reservation lost")
		}
	})
}
func TestRunReceiptDoesNotReleaseAccounts(t *testing.T) {
	a, now := runFixture(t)
	tx, _, _ := a.runFlowTick("w", now)
	if err := a.confirmTx(tx.ID, "hash", `{"status":"0x1"}`, "0x2", "0x1", 1, 1); err != nil {
		t.Fatal(err)
	}
	a.store.View(func(s model.State) {
		if s.Transactions[0].Status != "settling" || !transactionNodesBusy(s.Transactions, tx.FromNode, "") {
			t.Fatal("premature release")
		}
		reconcileInterruptedTasks(&s)
		if !transactionNodesBusy(s.Transactions, tx.ToNode, "") {
			t.Fatal("restart lost reservation")
		}
	})
}
func TestRunReadinessRequiresBothChainAndPrivateEvidence(t *testing.T) {
	x := readinessSample{TransactionHash: "tx", Hash: "block", Canonical: "block", Head: 12, Block: 11, Status: "0x1", Balance: "0xa", StateBlock: "0xb", CommitmentReady: true}
	if !validRunReadiness(x, "tx", "10", 2) {
		t.Fatal("valid evidence rejected")
	}
	for _, mutate := range []func(*readinessSample){func(x *readinessSample) { x.Canonical = "fork" }, func(x *readinessSample) { x.Head = 11 }, func(x *readinessSample) { x.Balance = "11" }, func(x *readinessSample) { x.CommitmentReady = false }, func(x *readinessSample) { x.StateBlock = "0xa" }, func(x *readinessSample) { x.Status = "0x0" }, func(x *readinessSample) { x.TransactionHash = "other" }} {
		bad := x
		mutate(&bad)
		if validRunReadiness(bad, "tx", "10", 2) {
			t.Fatal("unsafe evidence accepted")
		}
	}
}
func TestRunReportSeparatesCohortAndFixedWindow(t *testing.T) {
	start := time.Now()
	w := model.Workload{ID: "w", MeasurementStartedAt: start, MeasurementEndsAt: start.Add(time.Hour)}
	s := model.State{Transactions: []model.Transaction{{WorkloadID: "w", RunPhase: "warmup", Status: "confirmed", ConfirmedAt: start.Add(time.Second)}, {WorkloadID: "w", RunPhase: "measuring", Status: "confirmed", SubmittedAt: start.Add(time.Second), ConfirmedAt: start.Add(2 * time.Hour)}, {WorkloadID: "w", RunPhase: "measuring", Status: "unknown"}, {WorkloadID: "other", RunPhase: "measuring", Status: "confirmed"}}}
	r := runReport(s, w, start.Add(3*time.Hour))
	if r["windowSeconds"] != float64(3600) || r["measurementSubmissions"] != 2 || r["windowConfirmed"] != 1 || r["pendingSubmissions"] != 1 {
		t.Fatalf("wrong window: %v", r)
	}
}
func TestRunRejectsForgedAndInvalidConfiguration(t *testing.T) {
	a, _ := runFixture(t)
	rr := httptest.NewRecorder()
	a.createRun(rr, httptest.NewRequest("POST", "/api/workloads", nil), model.Workload{Strategy: "ready-pool", Type: "transfer", Value: "1", Mode: "rate", RatePerSecond: 1, DurationSeconds: 3600, WarmupSeconds: 600, Confirmations: 2, NodeIDs: []string{"a", "a"}, ExperimentID: "e"})
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "重复") {
		t.Fatal(rr.Body.String())
	}
}
