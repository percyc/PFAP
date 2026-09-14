package api

import (
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/pfap/lab/internal/model"
)

func validPublicRunReadiness(x readinessSample, hash string, confirmations int) bool {
	return confirmations > 0 && hash != "" && strings.EqualFold(x.TransactionHash, hash) && x.Hash != "" && strings.EqualFold(x.Hash, x.Canonical) && (x.Status == "0x1" || x.Status == "1") && x.Block > 0 && x.Head >= x.Block && x.Head-x.Block+1 >= uint64(confirmations)
}

// The quota belongs to the actual broadcaster, not to a Transfer payer.
// Reset the sequence at the formal phase boundary; offset five-slot cycles
// across nodes. Reservations count before RPC to prevent concurrent overshoot.
func mixedNextType(percent, index, count int) string {
	slot := (count + index%5) % 5
	k := percent / 20
	if (slot+1)*k/5 > slot*k/5 {
		return "transfer"
	}
	return "public"
}

func chooseRunTask(s model.State, e model.Experiment, w model.Workload, now time.Time) (string, model.Node, model.Node, bool) {
	if w.Type != "mixed" {
		p, r, ok := chooseRunPairAt(s, e, w, now)
		return "transfer", p, r, ok
	}
	counts := map[string]int{}
	last := map[string]time.Time{}
	for _, t := range s.Transactions {
		if t.WorkloadID != w.ID || t.RunPhase != w.Phase {
			continue
		}
		owner := t.FromNode
		if t.Type == "transfer" {
			owner = t.ToNode
		}
		counts[owner]++
		if t.SubmittedAt.After(last[owner]) {
			last[owner] = t.SubmittedAt
		}
	}
	nodes := runNodes(e, w)
	free := []model.Node{}
	for _, n := range nodes {
		if runTradingNode(n) && !runTemporarilyUnavailable(n) && runSampleFresh(n, now) && n.StateError == "" && n.PrivateStateError == "" && !transactionNodesBusy(s.Transactions, n.ID, "") {
			free = append(free, n)
		}
	}
	sort.SliceStable(free, func(i, j int) bool {
		if last[free[i].ID].Equal(last[free[j].ID]) {
			return free[i].Index < free[j].Index
		}
		return last[free[i].ID].Before(last[free[j].ID])
	})
	value, ok := amount(w.Value)
	if !ok {
		return "", model.Node{}, model.Node{}, false
	}
	for _, owner := range free {
		typ := mixedNextType(w.TransferPercent, owner.Index, counts[owner.ID])
		if typ == "public" {
			// Public receipts do not mutate the recipient's private state or nonce.
			// A deterministic ring avoids a single recipient accumulating all funds.
			for i, n := range nodes {
				if n.ID != owner.ID {
					continue
				}
				for step := 1; step < len(nodes); step++ {
					r := nodes[(i+step)%len(nodes)]
					if runTradingNode(r) && r.Account != "" {
						return typ, owner, r, true
					}
				}
			}
			continue
		}
		balance, valid := amount(owner.ZKBalance)
		if !valid || new(big.Int).Add(balance, value).BitLen() > 64 {
			continue
		}
		// Prefer a funded payer with the largest private balance to redistribute
		// liquidity; the payer's own quota is unchanged by this participation.
		var payer model.Node
		best := new(big.Int).Sub(value, big.NewInt(1))
		for _, candidate := range free {
			b, valid := amount(candidate.ZKBalance)
			if candidate.ID != owner.ID && valid && b.Cmp(best) > 0 {
				payer, best = candidate, b
			}
		}
		if payer.ID != "" {
			return typ, payer, owner, true
		}
	}
	return "", model.Node{}, model.Node{}, false
}
