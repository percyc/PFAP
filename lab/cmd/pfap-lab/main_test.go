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
	for _, id := range []string{"serverForm", "experimentForm", "transactionForm", "workloadForm", "consoleForm", "nodeConsole", "metrics"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Fatalf("missing UI element %s", id)
		}
	}
}
