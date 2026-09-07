package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func freshPreflightTestAPI(t *testing.T) (*API, string, model.Experiment) {
	t.Helper()
	a, statePath, server, _ := newDiskManagementTestAPI(t)
	var data bytes.Buffer
	compressed := gzip.NewWriter(&data)
	archive := tar.NewWriter(compressed)
	content := "test runtime, never executed"
	if err := archive.WriteHeader(&tar.Header{Name: "pfap-runtime/bin/geth", Mode: 0700, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(t.TempDir(), "runtime.tar.gz")
	if err := os.WriteFile(artifactPath, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	draft := model.Experiment{ID: "exp-fresh", Name: "fresh space check", Status: "draft", ArtifactPath: artifactPath, MinerCount: 2, NetworkID: 55123, Topology: "full-mesh", P2PPortBase: 30000, RPCPortBase: 40000, Placements: []model.Placement{{ServerID: server.ID, Count: 2}}, CreatedAt: time.Now().UTC()}
	saveTestState(t, a, func(s *model.State) {
		s.Experiments = []model.Experiment{draft}
		s.Transactions = []model.Transaction{{ID: "keep-transaction", ExperimentID: "history", Status: "unknown", Hash: "keep-hash"}}
		s.Workloads = []model.Workload{{ID: "keep-workload", ExperimentID: "history", Status: "interrupted"}}
	})
	return a, statePath, draft
}

func fakeDeploymentDiskFailure(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	scripts := filepath.Join(dir, "worker-scripts")
	// Capture but never execute the worker script, including any erroneous stop.
	fake := "#!/bin/sh\nprintf 'CALL\\n' >> \"$PFAP_PREFLIGHT_TEST_SCRIPTS\"\ncat >> \"$PFAP_PREFLIGHT_TEST_SCRIPTS\"\nprintf '[ERROR] simulated insufficient free disk space\\n'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PFAP_PREFLIGHT_TEST_SCRIPTS", scripts)
	return scripts
}

func assertOnlyReadOnlyDiskPreflight(t *testing.T, scriptsPath string) {
	t.Helper()
	data, err := os.ReadFile(scriptsPath)
	if err != nil {
		t.Fatal(err)
	}
	scripts := string(data)
	if strings.Count(scripts, "CALL\n") != 1 || !strings.Contains(scripts, "disk_required_kb=") {
		t.Fatalf("expected one disk check, got %q", scripts)
	}
	for _, forbidden := range []string{"mkdir", "kill", "unlink", "truncate", "network.sh", "--datadir"} {
		if strings.Contains(scripts, forbidden) {
			t.Fatalf("preflight failure attempted %q: %s", forbidden, scripts)
		}
	}
}

func TestDeploymentDiskFailureReturnsFreshExperimentToRetryableDraft(t *testing.T) {
	a, _, original := freshPreflightTestAPI(t)
	scripts := fakeDeploymentDiskFailure(t)
	before := recoveryTestState(a)
	exp, servers, operation, err := a.beginLifecycle(original.ID, "deploy")
	if err != nil || operation != "deploying" || len(exp.Nodes) != 2 || exp.ArtifactSHA == "" {
		t.Fatalf("fresh admission: %+v %s %v", exp, operation, err)
	}
	a.deploy(exp.ID, exp, servers)
	assertOnlyReadOnlyDiskPreflight(t, scripts)
	after := recoveryTestState(a)
	restored := after.Experiments[0]
	if restored.Status != "draft" || restored.ArtifactSHA != "" || len(restored.Nodes) != 0 || !restored.StartedAt.IsZero() || !strings.Contains(restored.Error, "simulated insufficient free disk space") {
		t.Fatalf("preflight poisoned fresh manifest: %+v", restored)
	}
	restored.Error = original.Error
	if !reflect.DeepEqual(restored, original) {
		t.Fatalf("preflight changed original configuration: before=%+v after=%+v", original, restored)
	}
	if !reflect.DeepEqual(after.Transactions, before.Transactions) || !reflect.DeepEqual(after.Workloads, before.Workloads) {
		t.Fatal("preflight changed historical work")
	}
	found := false
	for _, event := range after.Events {
		found = found || event.Kind == "preflight-failed"
	}
	if !found {
		t.Fatal("preflight rejection was not recorded")
	}
	retry, _, operation, err := a.beginLifecycle(original.ID, "deploy")
	if err != nil || operation != "deploying" || len(retry.Nodes) != 2 {
		t.Fatalf("fresh retry became resume-only: %s %+v %v", operation, retry, err)
	}
}

func TestGroupedDeploymentPreflightRestoresOnlyAdmittedPlan(t *testing.T) {
	a, _, original := freshPreflightTestAPI(t)
	scripts := fakeDeploymentDiskFailure(t)
	saveTestState(t, a, func(s *model.State) {
		base := s.Servers[0]
		aServer, bServer, cServer := base, base, base
		aServer.ID, aServer.HostGroup = "worker-a", "pve-one"
		bServer.ID, bServer.HostGroup = "worker-b", "pve-one"
		cServer.ID, cServer.HostGroup = "worker-c", "pve-two"
		s.Servers = []model.Server{aServer, bServer, cServer}
		s.Experiments[0].Placements = []model.Placement{{ServerID: aServer.ID, Count: 1}, {ServerID: bServer.ID, Count: 1}, {ServerID: cServer.ID, Count: 1}}
	})
	admitted, servers, operation, err := a.beginLifecycle(original.ID, "deploy")
	if err != nil || operation != "deploying" || len(admitted.Nodes) != 3 || !admitted.Nodes[0].IsMiner || admitted.Nodes[1].IsMiner || !admitted.Nodes[2].IsMiner {
		t.Fatalf("host-group plan not pinned: %+v %v", admitted, err)
	}
	// Labels can be edited later, but neither rollback identity nor the already
	// admitted worker snapshot may be recomputed from those new labels.
	saveTestState(t, a, func(s *model.State) { s.Servers[0].HostGroup = "changed" })
	a.deploy(admitted.ID, admitted, servers)
	assertOnlyReadOnlyDiskPreflight(t, scripts)
	restored := recoveryTestState(a).Experiments[0]
	if restored.Status != "draft" || len(restored.Nodes) != 0 || restored.ArtifactSHA != "" || restored.MinerCount != 2 || len(restored.Placements) != 3 {
		t.Fatalf("grouped plan cannot return to draft: %+v", restored)
	}
}

func TestDeploymentDraftRestorationRejectsChangedOrReusedState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*model.Experiment)
	}{
		{"interrupted", func(e *model.Experiment) { e.Status = "interrupted" }},
		{"resuming", func(e *model.Experiment) { e.Status = "resuming" }},
		{"different SHA", func(e *model.Experiment) { e.ArtifactSHA = strings.Repeat("b", 64) }},
		{"previously started", func(e *model.Experiment) { e.StartedAt = time.Now() }},
		{"original account", func(e *model.Experiment) { e.Nodes[0].Account = "original-account" }},
		{"live node", func(e *model.Experiment) { e.Nodes[0].Status = "running" }},
		{"different manifest", func(e *model.Experiment) { e.Nodes = e.Nodes[:1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, original := freshPreflightTestAPI(t)
			exp, _, _, err := a.beginLifecycle(original.ID, "deploy")
			if err != nil {
				t.Fatal(err)
			}
			saveTestState(t, a, func(s *model.State) { tc.change(&s.Experiments[0]) })
			before := recoveryTestState(a)
			if err := a.restoreDeploymentDraft(exp, errors.New("space unavailable")); err == nil {
				t.Fatal("changed or reused state was cleared")
			}
			if !reflect.DeepEqual(before, recoveryTestState(a)) {
				t.Fatal("rejected restoration still changed state")
			}
		})
	}
}

func TestDeploymentPreflightNeverStopsNodesWhenStateChangedOrSaveFailed(t *testing.T) {
	for _, failure := range []string{"interrupted", "storage failure"} {
		t.Run(failure, func(t *testing.T) {
			a, statePath, original := freshPreflightTestAPI(t)
			scripts := fakeDeploymentDiskFailure(t)
			exp, servers, _, err := a.beginLifecycle(original.ID, "deploy")
			if err != nil {
				t.Fatal(err)
			}
			if failure == "interrupted" {
				saveTestState(t, a, func(s *model.State) { s.Experiments[0].Status = "interrupted" })
			} else {
				if err := os.Rename(statePath, statePath+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(statePath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			before := recoveryTestState(a).Experiments[0]
			a.deploy(exp.ID, exp, servers)
			assertOnlyReadOnlyDiskPreflight(t, scripts)
			if after := recoveryTestState(a).Experiments[0]; !reflect.DeepEqual(before, after) {
				t.Fatalf("unsafe restoration: before=%+v after=%+v", before, after)
			}
			if failure == "storage failure" && !a.store.Health().Degraded {
				t.Fatal("failed restore did not expose storage error")
			}
		})
	}
}
