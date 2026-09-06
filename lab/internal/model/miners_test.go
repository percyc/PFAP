package model

import (
	"reflect"
	"sort"
	"testing"
)

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
