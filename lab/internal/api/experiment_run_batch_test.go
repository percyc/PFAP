package api

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func batchFixture(t *testing.T, alpha int) (*API, time.Time) {
	a, now := runFixture(t)
	saveTestState(t, a, func(s *model.State) {
		w, e := &s.Workloads[0], &s.Experiments[0]
		w.Type, w.TransferPercent, w.NodeIDs = "mixed", alpha, nil
		base, miner := e.Nodes[0], e.Nodes[len(e.Nodes)-1]
		e.Nodes = nil
		for i := 1; i <= 94; i++ {
			n := base
			n.ID, n.Account, n.Index = fmt.Sprintf("n%d", i), fmt.Sprintf("account%d", i), i
			e.Nodes = append(e.Nodes, n)
			w.NodeIDs = append(w.NodeIDs, n.ID)
		}
		e.Nodes = append(e.Nodes, miner)
	})
	return a, now
}

func TestRunBatchReservesAllAvailableAccounts(t *testing.T) {
	for alpha := 0; alpha <= 100; alpha += 20 {
		t.Run(fmt.Sprint(alpha), func(t *testing.T) {
			a, now := batchFixture(t, alpha)
			txs, done, err := a.runFlowBatch("w", 94, func() time.Time { return now })
			if err != nil || done || len(txs) == 0 {
				t.Fatalf("batch: %d %v %v", len(txs), done, err)
			}
			if (alpha == 0 && len(txs) != 94) || (alpha == 100 && len(txs) != 47) {
				t.Fatalf("idle capacity left unused: alpha=%d tasks=%d", alpha, len(txs))
			}
			occupied := map[string]bool{}
			for i, tx := range txs {
				if tx.Sequence != i+1 || tx.FromNode == tx.ToNode {
					t.Fatal("bad sequence or self transfer", tx)
				}
				actors := []string{tx.FromNode}
				owner := tx.FromNode
				if tx.Type == "transfer" {
					actors = append(actors, tx.ToNode)
					owner = tx.ToNode
				}
				var index int
				fmt.Sscanf(owner, "n%d", &index)
				if tx.Type != mixedNextType(alpha, index, 0) {
					t.Fatal("broadcaster quota changed", tx)
				}
				for _, actor := range actors {
					if occupied[actor] {
						t.Fatal("account reserved twice", actor)
					}
					occupied[actor] = true
				}
			}
			a.store.View(func(s model.State) {
				if len(s.Transactions) != len(txs) || s.Workloads[0].Submitted != len(txs) {
					t.Fatal("returned tasks differ from durable reservations")
				}
				if _, _, _, ok := chooseRunTask(s, s.Experiments[0], s.Workloads[0], now); ok {
					t.Fatal("batch left another schedulable task")
				}
			})
		})
	}
}

func TestRunConcurrentBatchesDoNotDoubleReserve(t *testing.T) {
	a, now := batchFixture(t, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := a.runFlowBatch("w", 94, func() time.Time { return now }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	a.store.View(func(s model.State) {
		if len(s.Transactions) != 94 {
			t.Fatalf("got %d reservations", len(s.Transactions))
		}
	})
}

func TestRunBatchRechecksDeadlineBetweenReservations(t *testing.T) {
	a, now := batchFixture(t, 0)
	saveTestState(t, a, func(s *model.State) {
		w := &s.Workloads[0]
		w.Phase, w.MeasurementStartedAt, w.MeasurementEndsAt = "measuring", now, now.Add(time.Second)
	})
	calls := 0
	txs, done, err := a.runFlowBatch("w", 94, func() time.Time {
		calls++
		if calls > 2 {
			return now.Add(2 * time.Second)
		}
		return now
	})
	if err != nil || !done || len(txs) != 1 {
		t.Fatalf("deadline batch: %d %v %v", len(txs), done, err)
	}
	a.store.View(func(s model.State) {
		if len(s.Transactions) != 1 || s.Workloads[0].Status != "draining" {
			t.Fatal("deadline not committed atomically")
		}
	})
}

func TestRunBatchFailedCommitReturnsNoTasks(t *testing.T) {
	a, now := runFixture(t)
	// This directory is inside the fixture's temporary store. It makes backup
	// replacement fail before the primary commit, without touching live data.
	if err := os.Mkdir(a.store.Health().BackupPath, 0700); err != nil {
		t.Fatal(err)
	}
	txs, _, err := a.runFlowBatch("w", 4, func() time.Time { return now })
	if err == nil || len(txs) != 0 {
		t.Fatalf("uncommitted tasks escaped: %d %v", len(txs), err)
	}
	a.store.View(func(s model.State) {
		if len(s.Transactions) != 0 || s.Workloads[0].Submitted != 0 {
			t.Fatal("failed batch became visible")
		}
	})
}
