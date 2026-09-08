package model

import (
	"fmt"
	"sort"
	"strings"
)

// MinerTarget is an audit snapshot; later host metadata edits cannot change it.
type MinerTarget struct {
	NodeID     string `json:"nodeId"`
	ServerID   string `json:"serverId"`
	LocalIndex int    `json:"localIndex"`
	HostGroup  string `json:"hostGroup"`
}

func SelectedMinerTargets(nodes []Node, servers map[string]Server) []MinerTarget {
	targets := make([]MinerTarget, 0)
	for _, node := range nodes {
		if node.IsMiner {
			targets = append(targets, MinerTarget{NodeID: node.ID, ServerID: node.ServerID, LocalIndex: node.LocalIndex, HostGroup: servers[node.ServerID].HostGroup})
		}
	}
	return targets
}

// EffectiveMinerCount preserves the original single-miner behavior for saved
// experiments that predate configurable mining. Explicit configuration is
// validated before an experiment is created or updated.
func EffectiveMinerCount(exp Experiment) int {
	if exp.MinerMode == "manual" {
		return len(exp.MinerSelections)
	}
	if exp.MinerCount == 0 {
		return 1
	}
	return exp.MinerCount
}

// NodeIsMiner separates the configured role from the live eth.mining reading.
// A stopped or unreachable miner retains its role so recovery can restore it.
func NodeIsMiner(exp Experiment, node Node) bool {
	return node.IsMiner || (exp.MinerMode != "manual" && exp.MinerCount == 0 && node.Index == 1)
}

// AssignMiners deterministically spreads miners over the configured servers:
// choose each server's lowest-index node first, then its second node, etc.
// It neither reorders nodes nor changes observed process/mining state.
func AssignMiners(nodes []Node, count int) error {
	return assignAutoMiners(nodes, count, nil)
}

func assignAutoMiners(nodes []Node, count int, servers map[string]Server) error {
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
	// Explicit physical hosts get one turn each. Unknown host identity retains
	// the historical per-server ordering without claiming hardware separation.
	type hostKey struct {
		known bool
		id    string
	}
	hostIndex := make(map[hostKey]int)
	var hosts [][][]int
	for _, group := range groups {
		serverID := nodes[group[0]].ServerID
		key := hostKey{id: serverID}
		if host := strings.ToLower(strings.TrimSpace(servers[serverID].HostGroup)); host != "" {
			key = hostKey{known: true, id: host}
		}
		index, ok := hostIndex[key]
		if !ok {
			index = len(hosts)
			hostIndex[key] = index
			hosts = append(hosts, nil)
		}
		hosts[index] = append(hosts[index], group)
	}
	var queues [][]int
	for _, host := range hosts {
		queues = append(queues, minerRoundRobin(host))
	}
	selection := minerRoundRobin(queues)
	for i := range nodes {
		nodes[i].IsMiner = false
	}
	for _, index := range selection[:count] {
		nodes[index].IsMiner = true
	}
	return nil
}

func minerRoundRobin(groups [][]int) []int {
	var order []int
	for round := 0; ; round++ {
		found := false
		for _, group := range groups {
			if round < len(group) {
				order = append(order, group[round])
				found = true
			}
		}
		if !found {
			return order
		}
	}
}

// ResolveMiners changes desired roles only after the full selection validates.
// Call it for explicit configuration changes/planning, never startup migration.
func ResolveMiners(nodes []Node, exp Experiment, servers map[string]Server) error {
	mode := exp.MinerMode
	if mode == "" {
		mode = "auto"
	}
	if mode == "auto" {
		if len(exp.MinerSelections) != 0 {
			return fmt.Errorf("automatic miner mode cannot include manual selections")
		}
		return assignAutoMiners(nodes, EffectiveMinerCount(exp), servers)
	}
	if mode != "manual" {
		return fmt.Errorf("miner mode must be auto or manual")
	}
	if len(exp.MinerSelections) == 0 {
		return fmt.Errorf("manual miner mode requires at least one selected node")
	}
	available := make(map[MinerSelection]int)
	for i, node := range nodes {
		key := MinerSelection{ServerID: node.ServerID, LocalIndex: node.LocalIndex}
		if _, exists := available[key]; exists {
			return fmt.Errorf("duplicate node placement for server %s node %d", key.ServerID, key.LocalIndex)
		}
		available[key] = i
	}
	selected := make(map[int]bool)
	for _, selection := range exp.MinerSelections {
		index, ok := available[selection]
		if selection.ServerID == "" || selection.LocalIndex < 1 || !ok {
			return fmt.Errorf("selected miner %s node %d is not in the experiment", selection.ServerID, selection.LocalIndex)
		}
		if selected[index] {
			return fmt.Errorf("duplicate selected miner %s node %d", selection.ServerID, selection.LocalIndex)
		}
		selected[index] = true
	}
	for i := range nodes {
		nodes[i].IsMiner = selected[i]
	}
	return nil
}

// ResolvePlannedMiners provides the same stable placement identities to draft
// selection, deployment and disk preflight before any worker is changed.
func ResolvePlannedMiners(exp Experiment, servers map[string]Server) ([]Node, error) {
	var nodes []Node
	seen := make(map[string]bool)
	for _, placement := range exp.Placements {
		if placement.ServerID == "" || seen[placement.ServerID] || placement.Count < 1 || placement.Count > 300 || len(nodes) > 300-placement.Count {
			return nil, fmt.Errorf("invalid or duplicate placement; experiment must contain 1–300 nodes")
		}
		seen[placement.ServerID] = true
		for local := 1; local <= placement.Count; local++ {
			index := len(nodes) + 1
			nodes = append(nodes, Node{ID: fmt.Sprintf("%s-n%d", exp.ID, index), Name: fmt.Sprintf("node-%d", index), ServerID: placement.ServerID, Index: index, LocalIndex: local, P2PPort: exp.P2PPortBase + index - 1, RPCPort: exp.RPCPortBase + index - 1, Status: "unknown", RuntimeSHA: exp.ArtifactSHA})
		}
	}
	if err := ResolveMiners(nodes, exp, servers); err != nil {
		return nil, err
	}
	return nodes, nil
}

// NormalizeMinerConfiguration validates a complete configuration and derives
// the manual count. Validation errors leave the caller's configuration intact.
func NormalizeMinerConfiguration(exp *Experiment) error {
	configured := *exp
	if configured.MinerMode == "" {
		configured.MinerMode = "auto"
	}
	configured.MinerCount = EffectiveMinerCount(configured)
	if len(configured.Placements) > 0 {
		if _, err := ResolvePlannedMiners(configured, nil); err != nil {
			return err
		}
	} else {
		nodes := append([]Node(nil), configured.Nodes...)
		if err := ResolveMiners(nodes, configured, nil); err != nil {
			return err
		}
	}
	exp.MinerMode, exp.MinerCount = configured.MinerMode, configured.MinerCount
	exp.MinerSelections = append([]MinerSelection(nil), configured.MinerSelections...)
	return nil
}
