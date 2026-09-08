package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"github.com/pfap/lab/internal/model"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExportsAreScopedAndRedacted(t *testing.T) {
	a, now := runFixture(t)
	saveTestState(t, a, func(s *model.State) {
		s.Experiments[0].Name = "<script>alert(1)</script>"
		s.Experiments[0].Error = "PRIVATE_TOKEN"
		s.Experiments[0].Nodes[0].RecoveryError = "PRIVATE_TOKEN"
		s.Workloads[0].Configuration = map[string]any{"identityFile": "PRIVATE_TOKEN", "servers": []map[string]any{{"id": "s", "systemInfo": "cpus=8\npassword=PRIVATE_TOKEN"}}}
		s.Workloads = append(s.Workloads, model.Workload{ID: "legacy", ExperimentID: "e"}, model.Workload{ID: "other", ExperimentID: "other"})
		s.Transactions = []model.Transaction{{ID: "included", ExperimentID: "e", WorkloadID: "w", Command: "PRIVATE_TOKEN", Receipt: "PRIVATE_TOKEN", Error: "PRIVATE_TOKEN"}, {ID: "manual", ExperimentID: "e"}, {ID: "excluded", ExperimentID: "other"}}
		s.Events = []model.Event{{ExperimentID: "e", Kind: "test", Message: "PRIVATE_TOKEN", Fields: map[string]any{"sn": "PRIVATE_TOKEN"}, At: now}}
	})
	var before []byte
	a.store.View(func(s model.State) {
		before, _ = json.Marshal(s)
		single, ok := buildResultExport(s, "", "w", time.Now())
		if !ok || len(single.Runs) != 1 || len(single.Transactions) != 1 {
			t.Fatal("single scope incorrect")
		}
		all, ok := buildResultExport(s, "e", "", time.Now())
		if !ok || len(all.Runs) != 2 || len(all.Transactions) != 2 {
			t.Fatal("whole scope incorrect")
		}
		data, _ := json.Marshal(all)
		if bytes.Contains(data, []byte("PRIVATE_TOKEN")) || bytes.Contains(data, []byte("excluded")) {
			t.Fatal("private data or foreign scope escaped")
		}
		for _, m := range all.Runs[1].Metrics {
			if m.Value != nil {
				t.Fatal("legacy window fabricated")
			}
		}
	})
	for _, format := range []string{"json", "html", "csv"} {
		rr := httptest.NewRecorder()
		a.exportResults(rr, httptest.NewRequest("GET", "/report?format="+format, nil), "e", "")
		if rr.Code != 200 {
			t.Fatal(rr.Body.String())
		}
		if bytes.Contains(rr.Body.Bytes(), []byte("PRIVATE_TOKEN")) {
			t.Fatal("secret escaped")
		}
		if format == "html" && strings.Contains(rr.Body.String(), "<script>") {
			t.Fatal("HTML injection")
		}
		if format == "csv" {
			z, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
			if err != nil {
				t.Fatal(err)
			}
			if len(z.File) != 7 {
				t.Fatalf("wrong archive: %d", len(z.File))
			}
			for _, f := range z.File {
				r, _ := f.Open()
				b, _ := io.ReadAll(r)
				r.Close()
				if bytes.Contains(b, []byte("PRIVATE_TOKEN")) {
					t.Fatal("secret in archive")
				}
			}
		}
	}
	a.store.View(func(s model.State) {
		after, _ := json.Marshal(s)
		if !bytes.Equal(before, after) {
			t.Fatal("export changed state")
		}
	})
}
func TestExportMissingDataAndFormulaProtection(t *testing.T) {
	a, now := runFixture(t)
	a.store.View(func(s model.State) {
		run := exportedRunMetrics(s, s.Workloads[0], now)
		for _, m := range run.Metrics {
			if m.Key == "p95LatencyMs" && m.Value != nil {
				t.Fatal("missing latency became zero")
			}
			if m.Explanation == "" || m.Unit == "" {
				t.Fatal("missing definition")
			}
		}
	})
	for _, s := range []string{"=SUM(A1)", " +1", "@cmd", "\tdata", "-cmd"} {
		if !strings.HasPrefix(csvText(s), "'") {
			t.Fatal("unescaped CSV formula")
		}
	}
	if csvText(nil) != "" {
		t.Fatal("missing value not blank")
	}
	rr := httptest.NewRecorder()
	a.exportResults(rr, httptest.NewRequest("GET", "/report?format=bad", nil), "e", "")
	if rr.Code != 400 {
		t.Fatal(rr.Code)
	}
	rr = httptest.NewRecorder()
	a.exportResults(rr, httptest.NewRequest("GET", "/report?format=json", nil), "", "missing")
	if rr.Code != 404 {
		t.Fatal(rr.Code)
	}
}
