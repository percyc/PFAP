package model

import (
	"fmt"
	"sort"
)

// EffectiveMinerCount preserves the original single-miner behavior for saved
// experiments that predate configurable mining. Explicit configuration is
// validated before an experiment is created or updated.
func EffectiveMinerCount(exp Experiment) int {
	if exp.MinerCount == 0 {
		return 1
	}
	return exp.MinerCount
}

// NodeIsMiner separates the configured role from the live eth.mining reading.
// A stopped or unreachable miner retains its role so recovery can restore it.
func NodeIsMiner(exp Experiment, node Node) bool {
	return node.IsMiner || (exp.MinerCount == 0 && node.Index == 1)
}

// AssignMiners deterministically spreads miners over the configured servers:
// choose each server's lowest-index node first, then its second node, etc.
// It neither reorders nodes nor changes observed process/mining state.
func AssignMiners(nodes []Node, count int) error {
	if count < 1 || count > len(nodes) {
		return fmt.Errorf("miner count must be between 1 and %d", len(nodes))
	}
	order := make([]int, len(nodes))
	for i := range nodes {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := nodes[order[i]], nodes[order[j]]
		if a.Index != b.Index {
			return a.Index < b.Index
		}
		return a.ID < b.ID
	})
	var groups [][]int
	serverGroup := make(map[string]int)
	for _, idx := range order {
		group, exists := serverGroup[nodes[idx].ServerID]
		if !exists {
			group = len(groups)
			serverGroup[nodes[idx].ServerID] = group
			groups = append(groups, nil)
		}
		groups[group] = append(groups[group], idx)
	}
	for i := range nodes {
		nodes[i].IsMiner = false
	}
	selected := 0
	for round := 0; selected < count; round++ {
		for _, group := range groups {
			if round < len(group) {
				nodes[group[round]].IsMiner = true
				selected++
				if selected == count {
					break
				}
			}
		}
	}
	return nil
}
