package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/store"
)

func TestServerAndStateAPI(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "lab.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(s).Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	body := `{"name":"worker-1","host":"10.0.0.1","user":"pfap","workDir":"/tmp/pfap"}`
	req := httptest.NewRequest(http.MethodPost, "/api/servers", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	b, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(b), "worker-1") {
		t.Fatalf("state=%s", b)
	}
}

func TestRejectsInvalidServer(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "lab.json"))
	h := New(s).Handler(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/servers", strings.NewReader(`{"name":"missing"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestMetricsAndWorkloadValidation(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "lab.json"))
	h := New(s).Handler(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"confirmed":0`) {
		t.Fatalf("metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/workloads", strings.NewReader(`{"type":"unknown","experimentId":"e","ratePerSecond":1,"durationSeconds":1}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("workload status=%d", rec.Code)
	}
}

func TestConsoleValidation(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "lab.json"))
	h := New(s).Handler(http.NotFoundHandler())
	for _, test := range []struct {
		body string
		want int
	}{
		{`{}`, http.StatusBadRequest},
		{`{"experimentId":"missing","nodeId":"missing","command":"eth.blockNumber"}`, http.StatusNotFound},
		{`{"experimentId":"missing","nodeId":"missing","command":"` + strings.Repeat("x", 16*1024+1) + `"}`, http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/console/execute", strings.NewReader(test.body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != test.want {
			t.Fatalf("status=%d want=%d body=%s", rec.Code, test.want, rec.Body.String())
		}
	}
}

func TestDeleteExperimentCascadesRelatedData(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "lab.json"))
	_ = s.Update(func(state *model.State) error {
		state.Experiments = []model.Experiment{{ID: "delete-me", Status: "stopped"}, {ID: "keep", Status: "draft"}}
		state.Transactions = []model.Transaction{{ID: "tx-delete", ExperimentID: "delete-me", Status: "confirmed"}, {ID: "tx-keep", ExperimentID: "keep"}}
		state.Workloads = []model.Workload{{ID: "work-delete", ExperimentID: "delete-me", Status: "completed"}}
		state.AccountSnapshots = []model.AccountSnapshot{{ID: "snapshot-delete", ExperimentID: "delete-me"}}
		state.Events = []model.Event{{ID: "event-delete", ExperimentID: "delete-me"}, {ID: "event-global"}}
		return nil
	})
	h := New(s).Handler(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodDelete, "/api/experiments/delete-me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	s.View(func(state model.State) {
		if len(state.Experiments) != 1 || state.Experiments[0].ID != "keep" || len(state.Transactions) != 1 || state.Transactions[0].ID != "tx-keep" || len(state.Workloads) != 0 || len(state.AccountSnapshots) != 0 || len(state.Events) != 1 || state.Events[0].ID != "event-global" {
			t.Fatalf("unexpected state after delete: %+v", state)
		}
	})
}

func TestDeleteExperimentRejectsActiveExperiment(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "lab.json"))
	_ = s.Update(func(state *model.State) error {
		state.Experiments = []model.Experiment{{ID: "active", Status: "running"}}
		return nil
	})
	h := New(s).Handler(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodDelete, "/api/experiments/active", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNextPortBaseAvoidsActiveRange(t *testing.T) {
	experiments := []model.Experiment{{P2PPortBase: 30000, RPCPortBase: 40000, Nodes: []model.Node{{}, {}}}}
	if got := nextPortBase(30000, 2, experiments, false); got != 30100 {
		t.Fatalf("p2p base=%d", got)
	}
	if got := nextPortBase(40000, 2, experiments, true); got != 40100 {
		t.Fatalf("rpc base=%d", got)
	}
}

func TestParseReceipt(t *testing.T) {
	out := "console output\n\"{\\\"blockNumber\\\":\\\"0x2a\\\",\\\"status\\\":\\\"0x1\\\",\\\"transactionHash\\\":\\\"0xabc\\\"}\"\n"
	raw, block, status, ok := parseReceipt(out)
	if !ok || block != "0x2a" || status != "0x1" || !strings.Contains(raw, "transactionHash") {
		t.Fatalf("ok=%v block=%q status=%q raw=%q", ok, block, status, raw)
	}
	if _, _, _, ok := parseReceipt(`"null"`); ok {
		t.Fatal("null receipt must not be confirmed")
	}
}

func TestPrivateStateOnChain(t *testing.T) {
	for _, block := range []string{"", "0", "0x0", " 0X0 "} {
		if privateStateOnChain(block) {
			t.Fatalf("%q must not be ready", block)
		}
	}
	for _, block := range []string{"1", "0x1", "3875"} {
		if !privateStateOnChain(block) {
			t.Fatalf("%q must be ready", block)
		}
	}
}

func TestTransactionNodesBusy(t *testing.T) {
	txs := []model.Transaction{{FromNode: "a", ToNode: "b", Status: "submitted"}, {FromNode: "c", Status: "confirmed"}}
	if !transactionNodesBusy(txs, "a", "") || !transactionNodesBusy(txs, "b", "d") || transactionNodesBusy(txs, "c", "d") {
		t.Fatal("active node detection is incorrect")
	}
}

func TestParseBlockValue(t *testing.T) {
	for input, want := range map[string]uint64{`"42"`: 42, "0x2a": 42, " 1061 ": 1061} {
		got, ok := parseBlockValue(input)
		if !ok || got != want {
			t.Fatalf("parseBlockValue(%q)=(%d,%v), want %d", input, got, ok, want)
		}
	}
}
