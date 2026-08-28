package main

import (
	"strings"
	"testing"
)

func TestEmbeddedWebAssets(t *testing.T) {
	for _, name := range []string{"web/index.html", "web/app.css", "web/accounts.css", "web/workloads.css", "web/transactions.css", "web/console.css", "web/app.js"} {
		b, err := web.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) < 100 {
			t.Fatalf("asset %s is unexpectedly small", name)
		}
	}
	b, _ := web.ReadFile("web/index.html")
	html := string(b)
	for _, id := range []string{"serverForm", "experimentForm", "transactionForm", "workloadForm", "consoleForm", "consolePreset", "nodeConsole", "overviewExperiment", "metrics"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Fatalf("missing UI element %s", id)
		}
	}
	for _, command := range []string{"eth.blockNumber", "admin.peers", "txpool.status", "eth.getAccountState()", "miner.start(1)"} {
		if !strings.Contains(html, command) {
			t.Fatalf("missing console preset %s", command)
		}
	}
	js, _ := web.ReadFile("web/app.js")
	for _, marker := range []string{"experimentSummary", "experimentDetail", "showExperiment", "newestFirst", "transactionExperimentId"} {
		if !strings.Contains(string(js), marker) {
			t.Fatalf("missing experiment UI behavior %s", marker)
		}
	}
	for _, className := range []string{"transaction-context", "transaction-primary", "transaction-secondary"} {
		if !strings.Contains(html, `class="`+className+`"`) {
			t.Fatalf("missing transaction layout %s", className)
		}
	}
}
