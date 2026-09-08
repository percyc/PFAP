package api

import (
	"github.com/pfap/lab/internal/model"
	"testing"
	"time"
)

func TestTransactionRPCAmount(t *testing.T) {
	for _, value := range []string{"1000000", "0xf4240"} {
		for _, kind := range []string{"mint", "redeem", "transfer", "public"} {
			if got, err := transactionRPCAmount(kind, value); err != nil || got != "0xf4240" {
				t.Fatalf("%s %s: %s %v", kind, value, got, err)
			}
		}
	}
	for _, value := range []string{"0", "-1", "abc", "18446744073709551616"} {
		if _, err := transactionRPCAmount("mint", value); err == nil {
			t.Fatal("invalid anonymous amount", value)
		}
	}
	if _, err := transactionRPCAmount("public", "18446744073709551616"); err != nil {
		t.Fatal(err)
	}
	if got, err := transactionRPCAmount("public", "0"); err != nil || got != "0x0" {
		t.Fatalf("zero-value public transaction: %s %v", got, err)
	}
}

func TestAmountDecodeNeverExecuted(t *testing.T) {
	fixture := model.Transaction{Type: "mint", Status: "unknown", ExecutionStage: "submit", FromNode: "n3", Value: "1000000", ProvingAt: time.Now(), SubmissionAttemptedAt: time.Now(), Command: `node-3: eth.sendMintTransaction({from:eth.accounts[0],value:"1000000"})`, Error: amountDecodePrefix + "\n    at web3.js:3143:20\n    at web3.js:6432:15\n    at web3.js:5081:36\n    at <anonymous>:1:1"}
	if !amountDecodeNeverExecuted(fixture) {
		t.Fatal("exact decoder rejection not recognized")
	}
	for _, mutate := range []func(*model.Transaction){
		func(t *model.Transaction) { t.Type = "transfer" },
		func(t *model.Transaction) { t.Hash = "0x123" },
		func(t *model.Transaction) { t.BroadcastAt = time.Now() },
		func(t *model.Transaction) { t.Error += "\nother output" },
		func(t *model.Transaction) { t.Command += ";eth.sendMintTransaction({})" },
		func(t *model.Transaction) { t.Value = "0xf4240" },
		func(t *model.Transaction) { t.Receipt = "{}" },
		func(t *model.Transaction) { t.Error = "context deadline exceeded" },
	} {
		tx := fixture
		mutate(&tx)
		if amountDecodeNeverExecuted(tx) {
			t.Fatal("ambiguous execution accepted", tx)
		}
	}
}
