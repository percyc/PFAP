package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func newMiningTestAPI(t *testing.T) *API {
	t.Helper()
	a, _ := newRecoveryTestAPI(t)
	if err := a.store.Update(func(s *model.State) error {
		s.Servers = []model.Server{{ID: "server-a", Host: "local"}, {ID: "server-b", Host: "local"}}
		s.Experiments[0].MinerCount = 1
		s.Experiments[0].Placements = []model.Placement{{ServerID: "server-a", Count: 2}, {ServerID: "server-b", Count: 1}}
		s.Experiments[0].Nodes = []model.Node{
			{ID: "node-1", Name: "node-1", ServerID: "server-a", Index: 1, LocalIndex: 1, Status: "running", IsMiner: true},
			{ID: "node-2", Name: "node-2", ServerID: "server-a", Index: 2, LocalIndex: 2, Status: "running"},
			{ID: "node-3", Name: "node-3", ServerID: "server-b", Index: 3, LocalIndex: 1, Status: "running"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCreateExperimentMinerCount(t *testing.T) {
	for _, tc := range []struct {
		name, field          string
		nodes, count, status int
	}{
		{name: "one node default", nodes: 1, count: 1, status: http.StatusCreated},
		{name: "multiple nodes default", nodes: 3, count: 2, status: http.StatusCreated},
		{name: "explicit one", field: `,"minerCount":1`, nodes: 3, count: 1, status: http.StatusCreated},
		{name: "all nodes", field: `,"minerCount":3`, nodes: 3, count: 3, status: http.StatusCreated},
		{name: "zero", field: `,"minerCount":0`, nodes: 3, status: http.StatusBadRequest},
		{name: "negative", field: `,"minerCount":-1`, nodes: 3, status: http.StatusBadRequest},
		{name: "too many", field: `,"minerCount":4`, nodes: 3, status: http.StatusBadRequest},
		{name: "fraction", field: `,"minerCount":1.5`, nodes: 3, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newMiningTestAPI(t)
			before := recoveryTestState(a)
			body := fmt.Sprintf(`{"name":"mining test","placements":[{"serverId":"server-a","count":%d}]%s}`, tc.nodes, tc.field)
			rec := httptest.NewRecorder()
			a.experiments(rec, httptest.NewRequest(http.MethodPost, "/api/experiments", strings.NewReader(body)))
			if rec.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
			}
			after := recoveryTestState(a)
			if tc.status != http.StatusCreated {
				if !reflect.DeepEqual(before, after) {
					t.Fatal("invalid creation modified persisted state")
				}
				return
			}
			var exp model.Experiment
			if err := json.Unmarshal(rec.Body.Bytes(), &exp); err != nil {
				t.Fatal(err)
			}
			if exp.MinerCount != tc.count || exp.Status != "draft" || len(after.Experiments) != len(before.Experiments)+1 {
				t.Fatalf("unexpected created experiment: %+v", exp)
			}
		})
	}
}

func TestMigrateMiningStatePreservesHistoricalSingleMiner(t *testing.T) {
	mining := true
	s := model.State{Experiments: []model.Experiment{
		{ID: "legacy", Nodes: []model.Node{{Index: 2, IsMiner: true, Mining: &mining}, {Index: 1, Mining: &mining}}},
		{ID: "configured", MinerCount: 2, Nodes: []model.Node{{Index: 1, IsMiner: true, Mining: &mining}, {Index: 3, IsMiner: true}}},
		{ID: "interrupted", MinerCount: 2, MiningStatus: "updating", Nodes: []model.Node{{Index: 1, IsMiner: true, Mining: &mining}}},
	}}
	migrateMiningState(&s)
	legacy := s.Experiments[0]
	if legacy.MinerCount != 1 || legacy.Nodes[0].IsMiner || !legacy.Nodes[1].IsMiner {
		t.Fatalf("legacy experiment changed its historical miner assignment: %+v", legacy)
	}
	configured := s.Experiments[1]
	if configured.MinerCount != 2 || !configured.Nodes[0].IsMiner || !configured.Nodes[1].IsMiner {
		t.Fatalf("migration changed configured miner roles: %+v", configured)
	}
	interrupted := s.Experiments[2]
	if interrupted.MiningStatus != "failed" || interrupted.MiningError == "" || interrupted.MinerCount != 2 || !interrupted.Nodes[0].IsMiner {
		t.Fatalf("interrupted update was not marked retryable without losing desired roles: %+v", interrupted)
	}
	for _, exp := range s.Experiments {
		for _, node := range exp.Nodes {
			if node.Mining != nil {
				t.Fatalf("persisted eth.mining must be resampled after controller startup: %+v", node)
			}
		}
	}
}

func TestBeginMiningUpdateAdmissionAndRoleSpread(t *testing.T) {
	a := newMiningTestAPI(t)
	before := recoveryTestState(a)
	exp, servers, err := a.beginMiningUpdate("experiment", 2)
	if err != nil {
		t.Fatal(err)
	}
	if exp.MinerCount != 2 || exp.MiningStatus != "updating" || !exp.Nodes[0].IsMiner || exp.Nodes[1].IsMiner || !exp.Nodes[2].IsMiner || len(servers) != 2 {
		t.Fatalf("unexpected admitted update: %+v servers=%+v", exp, servers)
	}
	// Returned slices must not alias persisted state, including unchanged nodes.
	exp.Nodes[0].Account = "caller mutation"
	after := recoveryTestState(a)
	if after.Experiments[0].Nodes[0].Account == "caller mutation" || !reflect.DeepEqual(before.Transactions, after.Transactions) {
		t.Fatal("mining admission aliases persisted nodes or changes transactions")
	}
	if _, _, _, err := a.beginNodeRecovery("experiment", "node-1"); err == nil {
		t.Fatal("node recovery admitted during a mining role update")
	}
}

func TestBeginMiningUpdateRejectsUnsafeChangesWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		count  int
		change func(*model.State)
	}{
		{name: "zero", count: 0},
		{name: "too many", count: 4},
		{name: "deploying", count: 2, change: func(s *model.State) { s.Experiments[0].Status = "deploying" }},
		{name: "stopping", count: 2, change: func(s *model.State) { s.Experiments[0].Status = "stopping" }},
		{name: "failed", count: 2, change: func(s *model.State) { s.Experiments[0].Status = "failed" }},
		{name: "duplicate operation", count: 2, change: func(s *model.State) { s.Experiments[0].MiningStatus = "updating" }},
		{name: "recovering node", count: 2, change: func(s *model.State) { s.Experiments[0].Nodes[1].Status = "recovering" }},
		{name: "missing server", count: 2, change: func(s *model.State) { s.Servers = s.Servers[:1] }},
		{name: "offline new miner", count: 2, change: func(s *model.State) { s.Experiments[0].Nodes[2].Status = "unreachable" }},
		{name: "offline removed miner", count: 1, change: func(s *model.State) {
			s.Experiments[0].MinerCount = 2
			s.Experiments[0].Nodes[2].IsMiner = true
			s.Experiments[0].Nodes[2].Status = "unreachable"
		}},
		{name: "no online target", count: 1, change: func(s *model.State) { s.Experiments[0].Nodes[0].Status = "unreachable" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newMiningTestAPI(t)
			if tc.change != nil {
				if err := a.store.Update(func(s *model.State) error { tc.change(s); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			before := recoveryTestState(a)
			if _, _, err := a.beginMiningUpdate("experiment", tc.count); err == nil {
				t.Fatal("unsafe update admitted")
			}
			if after := recoveryTestState(a); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected update changed stored state")
			}
		})
	}
}

func TestBeginMiningUpdateAllowsUnchangedOfflineMinerAndRetry(t *testing.T) {
	a := newMiningTestAPI(t)
	if err := a.store.Update(func(s *model.State) error {
		e := &s.Experiments[0]
		e.MinerCount, e.MiningStatus, e.MiningError = 2, "failed", "previous attempt interrupted"
		e.Nodes[0].Status, e.Nodes[2].IsMiner = "unreachable", true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	exp, _, err := a.beginMiningUpdate("experiment", 2)
	if err != nil || exp.MiningStatus != "updating" || exp.MiningError != "" || !exp.Nodes[0].IsMiner || exp.Nodes[0].Status != "unreachable" {
		t.Fatalf("same-count retry with an unchanged offline miner failed: %+v %v", exp, err)
	}
}

func TestBeginMiningUpdateDraftAndStoppedNeedNoOnlineProcess(t *testing.T) {
	for _, status := range []string{"draft", "stopped"} {
		t.Run(status, func(t *testing.T) {
			a := newMiningTestAPI(t)
			if err := a.store.Update(func(s *model.State) error {
				s.Experiments[0].Status = status
				for i := range s.Experiments[0].Nodes {
					s.Experiments[0].Nodes[i].Status = "stopped"
				}
				if status == "draft" {
					s.Experiments[0].Nodes = nil
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			exp, _, err := a.beginMiningUpdate("experiment", 2)
			if err != nil || exp.MinerCount != 2 || exp.MiningStatus == "updating" || exp.Status != status {
				t.Fatalf("offline configuration should not start a process: %+v %v", exp, err)
			}
		})
	}
}

func TestBeginMiningUpdateConcurrentAdmission(t *testing.T) {
	a := newMiningTestAPI(t)
	const callers = 10
	start, results := make(chan struct{}), make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, _, err := a.beginMiningUpdate("experiment", 2); results <- err }()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("admitted %d concurrent mining updates, want 1", successes)
	}
}

func TestMinerUpdateEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, method, body string
		status             int
	}{
		{name: "save draft", method: http.MethodPost, body: `{"minerCount":2}`, status: http.StatusOK},
		{name: "read method rejected", method: http.MethodGet, status: http.StatusMethodNotAllowed},
		{name: "fraction rejected", method: http.MethodPost, body: `{"minerCount":1.5}`, status: http.StatusBadRequest},
		{name: "missing rejected", method: http.MethodPost, body: `{}`, status: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newMiningTestAPI(t)
			if err := a.store.Update(func(s *model.State) error {
				s.Experiments[0].Status = "draft"
				s.Experiments[0].Nodes = nil
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			a.Handler(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(tc.method, "/api/experiments/experiment/miners", strings.NewReader(tc.body)))
			if rec.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status == http.StatusOK {
				exp := recoveryTestState(a).Experiments[0]
				if exp.MinerCount != 2 || exp.MiningStatus != "" {
					t.Fatalf("draft miner configuration not saved: %+v", exp)
				}
			}
		})
	}
}

func TestMiningObservationIsSampledAndClearedWhenUnavailable(t *testing.T) {
	for _, observed := range []string{"true", "false", "null"} {
		t.Run(observed, func(t *testing.T) {
			a, _ := newRecoveryTestAPI(t)
			exp, node, server, bin := configureRecoverySample(t, a, "0x10")
			data, err := os.ReadFile(filepath.Join(bin, "sample.json"))
			if err != nil {
				t.Fatal(err)
			}
			payload := strings.Replace(string(data), "{", `{"mining":`+observed+`,`, 1)
			if err := os.WriteFile(filepath.Join(bin, "sample.json"), []byte(payload), 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.sampleNode(context.Background(), exp, node, server, "monitor"); err != nil {
				t.Fatal(err)
			}
			actual := recoveryTestState(a).Experiments[0].Nodes[0].Mining
			if observed == "null" {
				if actual != nil {
					t.Fatalf("unknown mining was fabricated as %t", *actual)
				}
			} else if actual == nil || *actual != (observed == "true") {
				t.Fatalf("actual mining not sampled: %v", actual)
			}
			a.setNodeError(exp.ID, node.ID, "unreachable", "timeout")
			if recoveryTestState(a).Experiments[0].Nodes[0].Mining != nil {
				t.Fatal("offline node retained a stale actual mining state")
			}
		})
	}
}

func TestStaleMiningObservationCannotOverwriteRoleUpdate(t *testing.T) {
	for _, updating := range []bool{true, false} {
		t.Run(fmt.Sprintf("still_updating_%t", updating), func(t *testing.T) {
			a, _ := newRecoveryTestAPI(t)
			exp, node, server, bin := configureRecoverySample(t, a, "0x10")
			data, err := os.ReadFile(filepath.Join(bin, "sample.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "sample.json"), []byte(strings.Replace(string(data), "{", `{"mining":false,`, 1)), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "hold"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile(filepath.Join(bin, "release"), nil, 0600) })
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- a.sampleNode(ctx, exp, node, server, "monitor") }()
			deadline := time.Now().Add(2 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(bin, "started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("sample stub did not start")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := a.store.Update(func(s *model.State) error {
				mining := true
				e := &s.Experiments[0]
				e.Nodes[0].Mining = &mining
				e.MiningUpdatedAt = time.Now()
				if updating {
					e.MiningStatus = "updating"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("sample did not complete")
			}
			actual := recoveryTestState(a).Experiments[0].Nodes[0].Mining
			if actual == nil || !*actual {
				t.Fatal("stale monitor sample overwrote the newer verified mining state")
			}
		})
	}
}

const miningUpdateGeth = `#!/bin/bash
set -eu
bin="${0%/*}"
node="${2%/geth.ipc}"
node="${node##*/}"
expression="$4"
printf '%s %s\n' "$node" "$expression" >>"$bin/mining.log"
if [[ "$expression" == *miner.start* ]]; then
    if [ -f "$bin/fail-start-$node" ]; then echo 'mock start failure' >&2; exit 1; fi
    echo true
elif [[ "$expression" == *miner.stop* ]]; then
    if [ -f "$bin/fail-stop-$node" ]; then echo 'mock stop failure' >&2; exit 1; fi
    echo false
else
    echo 'unexpected command' >&2
    exit 1
fi
`

func TestRunMiningUpdateStartsTargetsBeforeStoppingExistingMiners(t *testing.T) {
	for _, failure := range []string{"", "start", "stop"} {
		name := failure
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			a, _ := newRecoveryTestAPI(t)
			_, _, _, bin := configureRecoverySample(t, a, "0x0")
			if err := os.WriteFile(filepath.Join(bin, "geth"), []byte(miningUpdateGeth), 0700); err != nil {
				t.Fatal(err)
			}
			if err := a.store.Update(func(s *model.State) error {
				e := &s.Experiments[0]
				e.MinerCount = 1
				e.Placements = []model.Placement{{ServerID: "server", Count: 2}}
				// Simulate a manually drifted previous role; desired assignment will move to node 1.
				mining := true
				e.Nodes[0].IsMiner = false
				e.Nodes[1].IsMiner, e.Nodes[1].Mining = true, &mining
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if failure != "" {
				node := "node1"
				if failure == "stop" {
					node = "node2"
				}
				if err := os.WriteFile(filepath.Join(bin, "fail-"+failure+"-"+node), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			exp, servers, err := a.beginMiningUpdate("experiment", 1)
			if err != nil {
				t.Fatal(err)
			}
			a.runMiningUpdate(exp, servers)
			after := recoveryTestState(a).Experiments[0]
			data, err := os.ReadFile(filepath.Join(bin, "mining.log"))
			if err != nil {
				t.Fatal(err)
			}
			log := string(data)
			start := strings.Index(log, "node1 miner.setEtherbase")
			stop := strings.Index(log, "miner.stop")
			if start < 0 || !after.Nodes[0].IsMiner || after.Nodes[1].IsMiner || after.MinerCount != 1 {
				t.Fatalf("target command/desired configuration lost: %+v log=%q", after, log)
			}
			if failure == "start" {
				if stop >= 0 || after.Nodes[1].Mining == nil || !*after.Nodes[1].Mining {
					t.Fatalf("failed target startup stopped or forgot existing miner: %+v log=%q", after, log)
				}
			} else if stop < start || after.Nodes[0].Mining == nil || !*after.Nodes[0].Mining {
				t.Fatalf("old miners stopped before target was verified: %+v log=%q", after, log)
			}
			if failure == "" {
				if after.MiningStatus != "" || after.MiningError != "" || after.Nodes[1].Mining == nil || *after.Nodes[1].Mining {
					t.Fatalf("successful update did not persist verified actual roles: %+v", after)
				}
			} else if after.MiningStatus != "failed" || after.MiningError == "" {
				t.Fatalf("partial failure incorrectly reported success: %+v", after)
			}
		})
	}
}
