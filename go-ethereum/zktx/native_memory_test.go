package zktx

import (
	"io/ioutil"
	"math/big"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// Real proofs, not broadcasts: this never opens or modifies a node account.
// Run in an isolated process; Go heap statistics do not include native memory.
func TestNativeProofMemorySoak(t *testing.T) {
	if os.Getenv("PFAP_TEST_MEMORY_SOAK") != "1" {
		t.Skip("opt-in native memory soak; requires the deployed proving keys")
	}
	if os.Getenv("PFAP_TEST_REAL_PROOFS") != "1" {
		t.Fatal("PFAP_TEST_REAL_PROOFS=1 is also required")
	}
	rounds := 10
	if v := os.Getenv("PFAP_TEST_MEMORY_ROUNDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 3 || n > 1000 {
			t.Fatal("PFAP_TEST_MEMORY_ROUNDS must be 3..1000")
		}
		rounds = n
	}
	var warmedRSS uint64
	for i := 0; i < rounds; i++ {
		TestTransferHistoricalWitnessRealProof(t)
		runtime.GC()
		status, err := ioutil.ReadFile("/proc/self/status")
		if err != nil {
			t.Fatal(err)
		}
		var memory []string
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "VmRSS:") || strings.HasPrefix(line, "VmHWM:") || strings.HasPrefix(line, "VmSwap:") {
				memory = append(memory, line)
			}
		}
		t.Logf("native-memory round=%d %s", i+1, strings.Join(memory, "; "))
		rss := nativeRSSKiB(t)
		if i == 1 {
			warmedRSS = rss
		}
		// After two warm rounds, changing Go caller threads must not retain
		// another complete proving working set (formerly ~180 MiB per step).
		if i > 1 && rss > warmedRSS+64*1024 {
			t.Fatalf("native RSS keeps growing after warmup: baseline=%d KiB current=%d KiB", warmedRSS, rss)
		}
	}
}

func nativeRSSKiB(t *testing.T) uint64 {
	t.Helper()
	status, err := ioutil.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			n, err := strconv.ParseUint(strings.Fields(line)[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatal("missing VmRSS")
	return 0
}

func TestSMTSnapshotMemorySoak(t *testing.T) {
	if os.Getenv("PFAP_TEST_MEMORY_SOAK") != "1" {
		t.Skip("opt-in 5000-commitment snapshot memory test")
	}
	tree := NewSMTSnapshot()
	defer tree.Close()
	for i := 1; i <= 5000; i++ {
		tree.Insert(common.BigToHash(big.NewInt(int64(i))))
	}
	before := nativeRSSKiB(t)
	root := tree.Root()
	for i := 0; i < 4; i++ {
		clone := tree.Clone()
		defer clone.Close()
		clone.Insert(common.BigToHash(big.NewInt(int64(10000 + i))))
		if tree.Root() != root || clone.Root() == root {
			t.Fatal("snapshot isolation failed")
		}
	}
	after := nativeRSSKiB(t)
	t.Logf("snapshot-memory commitments=5000 retainedClones=4 beforeKiB=%d afterKiB=%d", before, after)
	// A clone and one modified path must not copy the 5000-leaf native tree.
	if after > before+16*1024 {
		t.Fatal("snapshot copies exceed 16 MiB incremental RSS")
	}
}
