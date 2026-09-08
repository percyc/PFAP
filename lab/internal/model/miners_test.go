package model

import (
	"reflect"
	"sort"
	"testing"
)

func TestPlannedNodesCapacity300(t *testing.T) {
	for _, count := range []int{100, 300, 301} {
		e := Experiment{ID: "capacity", MinerCount: 1, Placements: []Placement{{ServerID: "a", Count: count}}}
		nodes, err := ResolvePlannedMiners(e, map[string]Server{"a": {ID: "a"}})
		if count <= 300 && (err != nil || len(nodes) != count) {
			t.Fatalf("count %d: nodes %d, %v", count, len(nodes), err)
		}
		if count > 300 && err == nil {
			t.Fatal("accepted oversized experiment")
		}
	}
}

func TestAssignMinersSpreadsAcrossServers(t *testing.T) {
	for _, tc := range []struct {
		count int
		want  []int
	}{
		{1, []int{1}},
		{2, []int{1, 4}},
		{3, []int{1, 4, 6}},
		{4, []int{1, 2, 4, 6}},
		{5, []int{1, 2, 4, 5, 6}},
		{6, []int{1, 2, 3, 4, 5, 6}},
	} {
		// Unsorted records, including a later placement on the first server.
		nodes := []Node{{ID: "n6", Index: 6, ServerID: "c"}, {ID: "n2", Index: 2, ServerID: "a"}, {ID: "n4", Index: 4, ServerID: "b"}, {ID: "n1", Index: 1, ServerID: "a"}, {ID: "n5", Index: 5, ServerID: "b"}, {ID: "n3", Index: 3, ServerID: "a"}}
		before := append([]Node(nil), nodes...)
		if err := AssignMiners(nodes, tc.count); err != nil {
			t.Fatal(err)
		}
		var got []int
		for i, node := range nodes {
			if node.ID != before[i].ID {
				t.Fatal("assignment reordered experiment nodes")
			}
			if node.IsMiner {
				got = append(got, node.Index)
			}
		}
		sort.Ints(got)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("count %d: miners %v, want %v", tc.count, got, tc.want)
		}
	}
}

func TestAssignMinersChangesRolesOnlyAndRejectsInvalidCounts(t *testing.T) {
	live := true
	nodes := []Node{{ID: "one", Index: 1, ServerID: "a", Status: "unreachable", IsMiner: true, Mining: &live}, {ID: "two", Index: 2, ServerID: "b", Status: "running", IsMiner: true}}
	for _, count := range []int{-1, 0, 3} {
		before := append([]Node(nil), nodes...)
		if err := AssignMiners(nodes, count); err == nil {
			t.Errorf("accepted invalid count %d", count)
		}
		if !reflect.DeepEqual(nodes, before) {
			t.Errorf("invalid count %d modified nodes", count)
		}
	}
	if err := AssignMiners(nil, 1); err == nil {
		t.Error("accepted miners without nodes")
	}
	if err := AssignMiners(nodes, 1); err != nil {
		t.Fatal(err)
	}
	if !nodes[0].IsMiner || nodes[1].IsMiner || nodes[0].Status != "unreachable" || nodes[0].Mining != &live || !*nodes[0].Mining {
		t.Fatalf("assignment changed observed state or retained stale role: %+v", nodes)
	}
}

func TestLegacyMinerRole(t *testing.T) {
	legacy := Experiment{}
	if EffectiveMinerCount(legacy) != 1 || !NodeIsMiner(legacy, Node{Index: 1}) || NodeIsMiner(legacy, Node{Index: 2}) {
		t.Fatal("legacy single-miner behavior changed")
	}
	configured := Experiment{MinerCount: 2}
	if EffectiveMinerCount(configured) != 2 || NodeIsMiner(configured, Node{Index: 1}) || !NodeIsMiner(configured, Node{Index: 3, IsMiner: true}) {
		t.Fatal("configured roles were replaced by index-based assumptions")
	}
}

func TestResolveMinersPhysicalHostsAndServerRoundRobin(t *testing.T) {
	nodes := []Node{{ID: "a1", Index: 1, ServerID: "a", LocalIndex: 1}, {ID: "a2", Index: 2, ServerID: "a", LocalIndex: 2}, {ID: "b1", Index: 3, ServerID: "b", LocalIndex: 1}, {ID: "b2", Index: 4, ServerID: "b", LocalIndex: 2}, {ID: "c1", Index: 5, ServerID: "c", LocalIndex: 1}, {ID: "c2", Index: 6, ServerID: "c", LocalIndex: 2}}
	servers := map[string]Server{"a": {HostGroup: "Host X"}, "b": {HostGroup: " host x "}, "c": {HostGroup: "host-y"}}
	for count, want := range [][]int{{1}, {1, 5}, {1, 3, 5}, {1, 3, 5, 6}, {1, 2, 3, 5, 6}, {1, 2, 3, 4, 5, 6}} {
		if err := ResolveMiners(nodes, Experiment{MinerMode: "auto", MinerCount: count + 1}, servers); err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, n := range nodes {
			if n.IsMiner {
				got = append(got, n.Index)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("count=%d got=%v want=%v", count+1, got, want)
		}
	}
	if err := ResolveMiners(nodes, Experiment{MinerCount: 2}, nil); err != nil {
		t.Fatal(err)
	}
	if !nodes[0].IsMiner || !nodes[2].IsMiner || nodes[4].IsMiner {
		t.Fatal("unknown host identity changed legacy server spread")
	}
}

func TestResolveManualMinersAndConfigurationValidation(t *testing.T) {
	live := true
	nodes := []Node{{ID: "one", Index: 1, ServerID: "a", LocalIndex: 1, IsMiner: true, Mining: &live}, {ID: "two", Index: 2, ServerID: "a", LocalIndex: 2}, {ID: "three", Index: 3, ServerID: "b", LocalIndex: 1}}
	exp := Experiment{MinerMode: "manual", MinerCount: 99, MinerSelections: []MinerSelection{{ServerID: "a", LocalIndex: 2}}, Placements: []Placement{{ServerID: "a", Count: 2}, {ServerID: "b", Count: 1}}}
	if err := NormalizeMinerConfiguration(&exp); err != nil {
		t.Fatal(err)
	}
	if exp.MinerCount != 1 {
		t.Fatalf("manual count not derived: %d", exp.MinerCount)
	}
	if err := ResolveMiners(nodes, exp, nil); err != nil {
		t.Fatal(err)
	}
	if nodes[0].IsMiner || !nodes[1].IsMiner || nodes[2].IsMiner || nodes[0].Mining != &live {
		t.Fatal("manual selection changed observations or selected wrong node")
	}
	for _, bad := range []Experiment{
		{MinerMode: "invalid", MinerCount: 1}, {MinerMode: "manual"},
		{MinerMode: "manual", MinerSelections: []MinerSelection{{"a", 2}, {"a", 2}}},
		{MinerMode: "manual", MinerSelections: []MinerSelection{{"b", 2}}},
		{MinerMode: "manual", MinerSelections: []MinerSelection{{"a", 0}}},
		{MinerMode: "auto", MinerCount: 1, MinerSelections: []MinerSelection{{"a", 2}}},
	} {
		before := append([]Node(nil), nodes...)
		if err := ResolveMiners(nodes, bad, nil); err == nil {
			t.Fatalf("invalid config accepted: %+v", bad)
		}
		if !reflect.DeepEqual(nodes, before) {
			t.Fatal("invalid selection changed nodes")
		}
	}
	plan, err := ResolvePlannedMiners(exp, nil)
	if err != nil || len(plan) != 3 || plan[1].LocalIndex != 2 || !plan[1].IsMiner || plan[0].IsMiner {
		t.Fatalf("manual plan: %+v %v", plan, err)
	}
}
