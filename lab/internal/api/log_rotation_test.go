package api

import (
	"testing"

	"github.com/pfap/lab/internal/model"
)

func TestLogRotationDefersTransactionsAndLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, expStatus, nodeStatus, miningStatus, txStatus, from, to string
		eligible                                                      bool
	}{
		{name: "idle running", expStatus: "running", nodeStatus: "running", eligible: true},
		{name: "stopped", expStatus: "stopped", nodeStatus: "stopped", eligible: true},
		{name: "offline", expStatus: "running", nodeStatus: "unreachable", eligible: true},
		{name: "queued sender", expStatus: "running", nodeStatus: "running", txStatus: "queued", from: "node"},
		{name: "active proof", expStatus: "running", nodeStatus: "running", txStatus: "proving", from: "node"},
		{name: "receiver", expStatus: "running", nodeStatus: "running", txStatus: "submitted", from: "another", to: "node"},
		{name: "unknown", expStatus: "running", nodeStatus: "running", txStatus: "unknown", from: "node"},
		{name: "confirmed", expStatus: "running", nodeStatus: "running", txStatus: "confirmed", from: "node", eligible: true},
		{name: "other node busy", expStatus: "running", nodeStatus: "running", txStatus: "unknown", from: "another", eligible: true},
		{name: "deploying", expStatus: "deploying", nodeStatus: "unknown"},
		{name: "resuming", expStatus: "resuming", nodeStatus: "unknown"},
		{name: "stopping", expStatus: "stopping", nodeStatus: "running"},
		{name: "recovering", expStatus: "running", nodeStatus: "recovering"},
		{name: "mining update", expStatus: "running", nodeStatus: "running", miningStatus: "updating"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := model.State{Servers: []model.Server{{ID: "worker"}}, Experiments: []model.Experiment{{ID: "exp", Status: tc.expStatus, MiningStatus: tc.miningStatus, Nodes: []model.Node{{ID: "node", ServerID: "worker", Status: tc.nodeStatus}}}}, Transactions: []model.Transaction{{Status: tc.txStatus, FromNode: tc.from, ToNode: tc.to}}}
			_, _, _, eligible := rotationCandidate(s, "node")
			if eligible != tc.eligible {
				t.Fatalf("eligible=%v want=%v", eligible, tc.eligible)
			}
			s.Servers = nil
			if _, _, _, ok := rotationCandidate(s, "node"); ok {
				t.Fatal("rotation without registered server")
			}
		})
	}
}
