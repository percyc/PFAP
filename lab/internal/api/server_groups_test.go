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
	"github.com/pfap/lab/internal/store"
)

func hostGroupTestAPI(t *testing.T) *API {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "lab.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	err = s.Update(func(s *model.State) error {
		s.Servers = []model.Server{
			{ID: "srv-a", Name: "worker-a", Host: "192.0.2.1", User: "pfap", Port: 22, WorkDir: "/srv/pfap", HostGroup: "old-host", Status: "online", LastCheck: now, LastSuccessAt: now, SystemInfo: "disk_available_kb=4096"},
			{ID: "srv-b", Name: "worker-b", Host: "192.0.2.2", User: "pfap", Port: 22, WorkDir: "/srv/pfap"},
		}
		s.Experiments = []model.Experiment{{ID: "exp", Status: "running", MinerCount: 1, Placements: []model.Placement{{ServerID: "srv-a", Count: 2}, {ServerID: "srv-b", Count: 1}}, Nodes: []model.Node{{ID: "n1", ServerID: "srv-a", Index: 1, LocalIndex: 1, Status: "running", IsMiner: true}, {ID: "n2", ServerID: "srv-a", Index: 2, LocalIndex: 2, Status: "running"}, {ID: "n3", ServerID: "srv-b", Index: 3, LocalIndex: 1, Status: "running"}}}}
		s.Transactions = []model.Transaction{{ID: "tx", ExperimentID: "exp", Status: "confirmed"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return &API{store: s, subscribers: map[chan model.Event]struct{}{}}
}

func groupRequest(a *API, method, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	a.Handler(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(string(b))))
	return rec
}

func TestHostGroupMetadataEditDoesNotChangeMiningOrCachedStatus(t *testing.T) {
	a := hostGroupTestAPI(t)
	before := recoveryTestState(a)
	body := map[string]any{"name": "renamed", "host": "192.0.2.1", "port": 22, "user": "pfap", "workDir": "/srv/pfap", "hostGroup": "  PVE-01  "}
	rec := groupRequest(a, http.MethodPut, "/api/servers/srv-a", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit %d %s", rec.Code, rec.Body.String())
	}
	after := recoveryTestState(a)
	if after.Servers[0].HostGroup != "PVE-01" || after.Servers[0].SystemInfo != before.Servers[0].SystemInfo || !after.Servers[0].LastSuccessAt.Equal(before.Servers[0].LastSuccessAt) {
		t.Fatal("metadata edit lost group or metrics")
	}
	if !reflect.DeepEqual(before.Experiments, after.Experiments) || !reflect.DeepEqual(before.Transactions, after.Transactions) {
		t.Fatal("metadata edit changed experiment or transaction")
	}
	delete(body, "hostGroup")
	rec = groupRequest(a, http.MethodPut, "/api/servers/srv-a", body)
	if rec.Code != http.StatusOK || recoveryTestState(a).Servers[0].HostGroup != "PVE-01" {
		t.Fatal("old client omitted field erased group")
	}
	body["hostGroup"] = ""
	rec = groupRequest(a, http.MethodPut, "/api/servers/srv-a", body)
	if rec.Code != http.StatusOK || recoveryTestState(a).Servers[0].HostGroup != "" {
		t.Fatal("explicit group clear failed")
	}
}

func TestBatchHostGroupAtomicAndMetadataOnly(t *testing.T) {
	a := hostGroupTestAPI(t)
	before := recoveryTestState(a)
	rec := groupRequest(a, http.MethodPost, "/api/servers/batch/host-group", map[string]any{"ids": []string{"srv-a", "srv-b"}, "hostGroup": "  宿主机 A  "})
	if rec.Code != http.StatusOK {
		t.Fatalf("batch %d %s", rec.Code, rec.Body.String())
	}
	after := recoveryTestState(a)
	for _, s := range after.Servers {
		if s.HostGroup != "宿主机 A" {
			t.Fatal("group missing")
		}
	}
	if !reflect.DeepEqual(before.Experiments, after.Experiments) || !reflect.DeepEqual(before.Transactions, after.Transactions) {
		t.Fatal("batch changed existing roles or transactions")
	}
	if len(after.Events) != len(before.Events)+1 || after.Events[len(after.Events)-1].Kind != "server-host-group" {
		t.Fatal("missing audit event")
	}
	rec = groupRequest(a, http.MethodPost, "/api/servers/batch/host-group", map[string]any{"ids": []string{"srv-a"}, "hostGroup": ""})
	if rec.Code != http.StatusOK || recoveryTestState(a).Servers[0].HostGroup != "" || recoveryTestState(a).Servers[1].HostGroup != "宿主机 A" {
		t.Fatal("clear changed unselected host")
	}
}

func TestBatchHostGroupPersistenceFailureLeavesAllGroupsUnchanged(t *testing.T) {
	a := hostGroupTestAPI(t)
	before := recoveryTestState(a)
	file := filepath.Join(a.store.DataDir(), "lab.json")
	if err := os.Rename(file, file+".protected"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	rec := groupRequest(a, http.MethodPost, "/api/servers/batch/host-group", map[string]any{"ids": []string{"srv-a", "srv-b"}, "hostGroup": "pve-new"})
	if rec.Code != http.StatusConflict || !a.store.Health().Degraded {
		t.Fatalf("storage failure not reported: %d %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(before, recoveryTestState(a)) {
		t.Fatal("storage failure partially changed groups or audit history")
	}
}

func TestBatchHostGroupRejectsInvalidRequestsWithoutPartialWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   map[string]any
		busy   bool
		status int
	}{
		{"missing group", map[string]any{"ids": []string{"srv-a"}}, false, 400},
		{"null group", map[string]any{"ids": []string{"srv-a"}, "hostGroup": nil}, false, 400},
		{"empty ids", map[string]any{"ids": []string{}, "hostGroup": "pve"}, false, 400},
		{"duplicate", map[string]any{"ids": []string{"srv-a", "srv-a"}, "hostGroup": "pve"}, false, 400},
		{"missing server", map[string]any{"ids": []string{"srv-a", "missing"}, "hostGroup": "pve"}, false, 409},
		{"too long", map[string]any{"ids": []string{"srv-a"}, "hostGroup": strings.Repeat("宿", 81)}, false, 400},
		{"control", map[string]any{"ids": []string{"srv-a"}, "hostGroup": "pve\nname"}, false, 400},
		{"format control", map[string]any{"ids": []string{"srv-a"}, "hostGroup": "pve\u202ename"}, false, 400},
		{"maintenance", map[string]any{"ids": []string{"srv-a", "srv-b"}, "hostGroup": "pve"}, true, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := hostGroupTestAPI(t)
			before := recoveryTestState(a)
			if tc.busy {
				a.serverMaintenance.Store("srv-b", true)
			}
			rec := groupRequest(a, http.MethodPost, "/api/servers/batch/host-group", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if !reflect.DeepEqual(before, recoveryTestState(a)) {
				t.Fatal("rejected batch mutated state")
			}
		})
	}
}

func TestNewServersRecordExplicitHostGroup(t *testing.T) {
	a := hostGroupTestAPI(t)
	rec := groupRequest(a, http.MethodPost, "/api/servers/batch", map[string]any{"hosts": []string{"192.0.2.11", "192.0.2.12"}, "user": "pfap", "hostGroup": " pve-02 "})
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	for _, s := range recoveryTestState(a).Servers[2:] {
		if s.HostGroup != "pve-02" {
			t.Fatal("batch group missing")
		}
	}
	rec = groupRequest(a, http.MethodPost, "/api/servers", map[string]any{"host": "192.0.2.13", "name": "new", "user": "pfap", "hostGroup": "pve-03"})
	if rec.Code != http.StatusCreated || recoveryTestState(a).Servers[4].HostGroup != "pve-03" {
		t.Fatalf("single group %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateExperimentExplicitMinerSelections(t *testing.T) {
	a := hostGroupTestAPI(t)
	body := map[string]any{"name": "manual", "placements": []model.Placement{{ServerID: "srv-a", Count: 2}, {ServerID: "srv-b", Count: 1}}, "minerMode": "manual", "minerSelections": []model.MinerSelection{{ServerID: "srv-a", LocalIndex: 2}, {ServerID: "srv-b", LocalIndex: 1}}}
	rec := groupRequest(a, http.MethodPost, "/api/experiments", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("manual create %d %s", rec.Code, rec.Body.String())
	}
	s := recoveryTestState(a)
	e := s.Experiments[len(s.Experiments)-1]
	if e.MinerMode != "manual" || e.MinerCount != 2 || len(e.MinerSelections) != 2 || len(e.Nodes) != 0 || e.Status != "draft" {
		t.Fatalf("bad manual configuration %+v", e)
	}
	for _, selections := range [][]model.MinerSelection{nil, {{ServerID: "srv-a", LocalIndex: 0}}, {{ServerID: "srv-a", LocalIndex: 3}}, {{ServerID: "unknown", LocalIndex: 1}}, {{ServerID: "srv-a", LocalIndex: 1}, {ServerID: "srv-a", LocalIndex: 1}}} {
		body["minerSelections"] = selections
		before := recoveryTestState(a)
		rec = groupRequest(a, http.MethodPost, "/api/experiments", body)
		if rec.Code != http.StatusBadRequest || !reflect.DeepEqual(before, recoveryTestState(a)) {
			t.Fatalf("bad manual target accepted %d %s", rec.Code, rec.Body.String())
		}
	}
}
