package orchestrator

import (
	"strings"
	"testing"
)

func TestBlockTimingMarkers(t *testing.T) {
	hash := "0x" + strings.Repeat("a", 64)
	m := ParseBlockValidationTimings("other duration=23\nPFAP_BLOCK_EXECUTION_VALIDATION hash=" + hash + " us=123\n")
	if len(m) != 1 || m[hash] != 123 {
		t.Fatal(m)
	}
	if len(ParseBlockValidationTimings("no marker")) != 0 {
		t.Fatal("fabricated sample")
	}
}
