package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/orchestrator"
	"github.com/pfap/lab/internal/store"
)

func diskValidationFixture(now time.Time) (model.State, model.Server, diskPlan, orchestrator.CacheTarget) {
	server := model.Server{ID: "worker-current", Name: "worker", Host: "worker.invalid", Port: 22, User: "pfap", WorkDir: "/srv/pfap-lab", IdentityFile: "/keys/id_ed25519", KnownHostsFile: "/keys/known_hosts"}
	target := orchestrator.CacheTarget{ExperimentID: "exp-stopped", ServerID: "worker-historical", Path: server.WorkDir + "/experiments/exp-stopped/worker-historical/ethash", Bytes: 4096, Files: 1, Fingerprint: strings.Repeat("a", 64)}
	exp := model.Experiment{ID: target.ExperimentID, Status: "stopped", Placements: []model.Placement{{ServerID: target.ServerID, Count: 1}, {ServerID: "worker-other", Count: 1}}, Nodes: []model.Node{{ID: "n1", ServerID: target.ServerID, Status: "stopped"}, {ID: "n2", ServerID: "worker-other", Status: "stopped"}}}
	return model.State{Servers: []model.Server{server}, Experiments: []model.Experiment{exp}}, server, diskPlan{Server: server, CheckedAt: now, Targets: []orchestrator.CacheTarget{target}}, target
}

func TestDiskReviewRequiresFreshMatchingServerConfiguration(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		change func(*model.State, *model.Server, *diskPlan)
	}{
		{"expired", func(_ *model.State, _ *model.Server, p *diskPlan) {
			p.CheckedAt = now.Add(-diskPlanLifetime - time.Nanosecond)
		}},
		{"future", func(_ *model.State, _ *model.Server, p *diskPlan) { p.CheckedAt = now.Add(time.Nanosecond) }},
		{"different id", func(_ *model.State, s *model.Server, _ *diskPlan) { s.ID = "new-id" }},
		{"different host", func(_ *model.State, s *model.Server, _ *diskPlan) { s.Host = "changed.invalid" }},
		{"different port", func(_ *model.State, s *model.Server, _ *diskPlan) { s.Port = 2222 }},
		{"different user", func(_ *model.State, s *model.Server, _ *diskPlan) { s.User = "changed" }},
		{"different directory", func(_ *model.State, s *model.Server, _ *diskPlan) { s.WorkDir = "/srv/other" }},
		{"different identity", func(_ *model.State, s *model.Server, _ *diskPlan) { s.IdentityFile = "/keys/other" }},
		{"different trust file", func(_ *model.State, s *model.Server, _ *diskPlan) { s.KnownHostsFile = "/keys/other_hosts" }},
		{"unregistered server", func(s *model.State, _ *model.Server, _ *diskPlan) { s.Servers = nil }},
		{"changed stored configuration", func(s *model.State, _ *model.Server, _ *diskPlan) { s.Servers[0].WorkDir = "/srv/changed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, server, plan, target := diskValidationFixture(now)
			tc.change(&state, &server, &plan)
			if selected, err := validateDiskTargets(state, server, plan, []orchestrator.CacheTarget{target}, now); err == nil || len(selected) != 0 {
				t.Fatalf("unsafe review accepted: targets=%+v error=%v", selected, err)
			}
		})
	}
	state, server, plan, target := diskValidationFixture(now)
	plan.CheckedAt = now.Add(-diskPlanLifetime)
	server.Name = "cosmetic rename"
	if _, err := validateDiskTargets(state, server, plan, []orchestrator.CacheTarget{target}, now); err != nil {
		t.Fatalf("review at lifetime boundary with same connection should remain valid: %v", err)
	}
}

func TestDiskReviewUsesOnlyReviewedIdentitiesAndByteCounts(t *testing.T) {
	now := time.Now()
	state, server, plan, target := diskValidationFixture(now)
	for _, tc := range []struct {
		name   string
		change func(*orchestrator.CacheTarget)
	}{
		{"unreviewed path", func(c *orchestrator.CacheTarget) { c.Path += "/../node1" }},
		{"unreviewed experiment", func(c *orchestrator.CacheTarget) { c.ExperimentID = "different" }},
		{"forged placement", func(c *orchestrator.CacheTarget) { c.ServerID = server.ID }},
		{"changed fingerprint", func(c *orchestrator.CacheTarget) { c.Fingerprint = strings.Repeat("b", 64) }},
		{"missing fingerprint", func(c *orchestrator.CacheTarget) { c.Fingerprint = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requested := target
			tc.change(&requested)
			if _, err := validateDiskTargets(state, server, plan, []orchestrator.CacheTarget{requested}, now); err == nil {
				t.Fatal("unreviewed target accepted")
			}
		})
	}
	for _, targets := range [][]orchestrator.CacheTarget{nil, {target, target}, make([]orchestrator.CacheTarget, 101)} {
		if _, err := validateDiskTargets(state, server, plan, targets, now); err == nil {
			t.Fatalf("invalid target count/duplicates accepted: %d", len(targets))
		}
	}
	forged := target
	forged.Bytes, forged.Files = 1<<60, 99999
	selected, err := validateDiskTargets(state, server, plan, []orchestrator.CacheTarget{forged}, now)
	if err != nil || !reflect.DeepEqual(selected, []orchestrator.CacheTarget{target}) {
		t.Fatalf("client counts replaced reviewed accounting: %+v, %v", selected, err)
	}
	if selected[0].ServerID == server.ID {
		t.Fatal("historical placement was rewritten to the re-registered server ID")
	}
}

func TestDiskReviewRequiresEveryRecordedNodeStoppedAndNoUncertainTransaction(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		change func(*model.State)
	}{
		{"unregistered experiment", func(s *model.State) { s.Experiments = nil }},
		{"running experiment", func(s *model.State) { s.Experiments[0].Status = "running" }},
		{"interrupted experiment", func(s *model.State) { s.Experiments[0].Status = "interrupted" }},
		{"unconfirmed stop", func(s *model.State) { s.Experiments[0].Status = "stop-failed" }},
		{"miner update", func(s *model.State) { s.Experiments[0].MiningStatus = "updating" }},
		{"no recorded nodes", func(s *model.State) { s.Experiments[0].Nodes = nil }},
		{"local node unknown", func(s *model.State) { s.Experiments[0].Nodes[0].Status = "unknown" }},
		{"other server node running", func(s *model.State) { s.Experiments[0].Nodes[1].Status = "running" }},
		{"placement missing", func(s *model.State) { s.Experiments[0].Placements = nil }},
		{"placement empty", func(s *model.State) { s.Experiments[0].Placements[0].Count = 0 }},
		{"no node for placement", func(s *model.State) { s.Experiments[0].Nodes[0].ServerID = "different" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, server, plan, target := diskValidationFixture(now)
			tc.change(&state)
			if _, err := validateDiskTargets(state, server, plan, []orchestrator.CacheTarget{target}, now); err == nil {
				t.Fatal("unsafe experiment accepted")
			}
		})
	}
	for _, status := range []string{"queued", "proving", "submitted", "unknown"} {
		t.Run("transaction "+status, func(t *testing.T) {
			state, server, plan, target := diskValidationFixture(now)
			state.Transactions = []model.Transaction{{ExperimentID: target.ExperimentID, Status: status}}
			if _, err := validateDiskTargets(state, server, plan, []orchestrator.CacheTarget{target}, now); err == nil {
				t.Fatal("pending transaction accepted")
			}
		})
	}
	state, server, plan, target := diskValidationFixture(now)
	state.Experiments[0].Status = "failed"
	state.Transactions = []model.Transaction{{ExperimentID: target.ExperimentID, Status: "confirmed"}, {ExperimentID: "other", Status: "unknown"}}
	if _, err := validateDiskTargets(state, server, plan, []orchestrator.CacheTarget{target}, now); err != nil {
		t.Fatalf("verified stopped failed experiment was blocked: %v", err)
	}
}

func newDiskManagementTestAPI(t *testing.T) (*API, string, model.Server, orchestrator.CacheTarget) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "lab.json")
	s, err := store.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	state, server, plan, target := diskValidationFixture(time.Now())
	if err := s.Update(func(s *model.State) error { *s = state; return nil }); err != nil {
		t.Fatal(err)
	}
	// No monitors and no real worker commands in API state-machine tests.
	a := &API{store: s, subscribers: map[chan model.Event]struct{}{}}
	a.diskPlans.Store(server.ID, plan)
	return a, filename, server, target
}

func diskCleanupTestRequest(t *testing.T, a *API, serverID string, targets []orchestrator.CacheTarget) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"targets": targets})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.Handler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/servers/"+serverID+"/disk/cleanup", strings.NewReader(string(body))))
	return w
}

func fakeDiskWorker(t *testing.T, reply string, exitCode string) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "invocations")
	// Ignore the submitted script entirely. No DAG files are touched.
	script := "#!/bin/sh\ncat >/dev/null\nprintf x >> \"$PFAP_DISK_TEST_MARKER\"\nprintf '%s\\n' \"$PFAP_DISK_TEST_REPLY\"\nexit \"$PFAP_DISK_TEST_EXIT\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PFAP_DISK_TEST_MARKER", marker)
	t.Setenv("PFAP_DISK_TEST_REPLY", reply)
	t.Setenv("PFAP_DISK_TEST_EXIT", exitCode)
	return marker
}

func assertDiskWorkerNotCalled(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("worker command executed unexpectedly: %v", err)
	}
}

func TestDiskCleanupRejectsLifecycleLockAndMissingReviewBeforeWorker(t *testing.T) {
	a, _, server, target := newDiskManagementTestAPI(t)
	marker := fakeDiskWorker(t, "dag-done\t4096\t1", "0")
	a.lifecycleMu.Lock()
	w := diskCleanupTestRequest(t, a, server.ID, []orchestrator.CacheTarget{target})
	a.lifecycleMu.Unlock()
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "正在执行实验操作") {
		t.Fatalf("lifecycle conflict: %d %s", w.Code, w.Body.String())
	}
	a.diskPlans.Delete(server.ID)
	w = diskCleanupTestRequest(t, a, server.ID, []orchestrator.CacheTarget{target})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "请先检查磁盘") {
		t.Fatalf("unreviewed cleanup: %d %s", w.Code, w.Body.String())
	}
	assertDiskWorkerNotCalled(t, marker)
}

func TestDiskCleanupPersistenceFailureNeverExecutesWorker(t *testing.T) {
	a, filename, server, target := newDiskManagementTestAPI(t)
	marker := fakeDiskWorker(t, "dag-done\t4096\t1", "0")
	// A directory forces atomic persistence to fail even under root.
	if err := os.Rename(filename, filename+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filename, 0700); err != nil {
		t.Fatal(err)
	}
	w := diskCleanupTestRequest(t, a, server.ID, []orchestrator.CacheTarget{target})
	if w.Code != http.StatusConflict {
		t.Fatalf("failed save admitted cleanup: %d %s", w.Code, w.Body.String())
	}
	assertDiskWorkerNotCalled(t, marker)
	if !a.store.Health().Degraded {
		t.Fatal("failed intent persistence did not degrade storage health")
	}
	a.store.View(func(s model.State) {
		if len(s.Events) != 0 {
			t.Fatal("unpersisted cleanup intent became visible")
		}
	})
	if _, ok := a.diskPlans.Load(server.ID); !ok {
		t.Fatal("review consumed despite no action")
	}
	if _, busy := a.serverMaintenance.Load(server.ID); busy {
		t.Fatal("maintenance lock leaked after failed save")
	}
	if !a.lifecycleMu.TryLock() {
		t.Fatal("lifecycle lock leaked after failed save")
	}
	a.lifecycleMu.Unlock()
}

func TestDiskCleanupReviewIsOneShotIncludingPartialFailure(t *testing.T) {
	for _, tc := range []struct {
		name, reply, exit string
		wantError         bool
	}{
		{"success", "dag-done\t4096\t1", "0", false},
		{"partial", "dag-progress\t4096\t1", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, server, target := newDiskManagementTestAPI(t)
			marker := fakeDiskWorker(t, tc.reply, tc.exit)
			requestTarget := target
			requestTarget.Bytes = 1 << 60
			w := diskCleanupTestRequest(t, a, server.ID, []orchestrator.CacheTarget{requestTarget})
			if w.Code != http.StatusOK {
				t.Fatalf("cleanup status=%d body=%s", w.Code, w.Body.String())
			}
			var body struct {
				Results    []diskCleanupResult `json:"results"`
				FreedBytes int64               `json:"freedBytes"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.FreedBytes != 4096 || len(body.Results) != 1 || body.Results[0].Path != target.Path || body.Results[0].ExperimentID != target.ExperimentID || (body.Results[0].Error != "") != tc.wantError {
				t.Fatalf("flattened accounting=%+v", body)
			}
			if _, ok := a.diskPlans.Load(server.ID); ok {
				t.Fatal("executed review remained reusable")
			}
			w = diskCleanupTestRequest(t, a, server.ID, []orchestrator.CacheTarget{target})
			if w.Code != http.StatusConflict {
				t.Fatalf("replayed cleanup: %d %s", w.Code, w.Body.String())
			}
			if content, err := os.ReadFile(marker); err != nil || string(content) != "x" {
				t.Fatalf("worker invocation count=%q error=%v", content, err)
			}
			a.store.View(func(s model.State) {
				if len(s.Events) != 2 || s.Events[0].Kind != "disk-cleanup" || s.Events[1].Kind != "disk-cleanup" {
					t.Fatalf("cleanup intent/results not recorded: %+v", s.Events)
				}
			})
		})
	}
}

func TestDiskMaintenanceBlocksServerEditsAndAtomicDeletion(t *testing.T) {
	a, _, server, _ := newDiskManagementTestAPI(t)
	a.serverMaintenance.Store(server.ID, true)
	before := recoveryTestState(a)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/servers/" + server.ID, `{"name":"renamed","host":"worker.invalid","port":22,"user":"pfap","workDir":"/srv/pfap-lab","identityFile":"/keys/id_ed25519"}`},
		{http.MethodDelete, "/api/servers/" + server.ID, ""},
		{http.MethodPost, "/api/servers/batch/delete", `{"ids":["worker-current"]}`},
	} {
		w := httptest.NewRecorder()
		a.Handler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "磁盘维护") {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	if !reflect.DeepEqual(before, recoveryTestState(a)) {
		t.Fatal("server maintenance allowed configuration changes")
	}
}
