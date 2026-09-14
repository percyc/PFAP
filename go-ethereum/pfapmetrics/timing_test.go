package pfapmetrics

import (
	"testing"
	"time"
)

func TestFirstBroadcastAndAdmission(t *testing.T) {
	start := time.Now()
	Broadcast("test", start)
	Broadcast("test", start.Add(time.Second))
	if !broadcasts["test"].Equal(start) {
		t.Fatal("later send replaced origin")
	}
	Broadcast("test", start.Add(-time.Millisecond))
	if !broadcasts["test"].Equal(start.Add(-time.Millisecond)) {
		t.Fatal("earlier successful send lost")
	}
	Admission("test", start, start.Add(time.Millisecond))
	Admission("test", start, start.Add(time.Second))
	if !admissions["test"] {
		t.Fatal("admission not recorded")
	}
	Included("test", "block", start.Add(time.Second))
	Header("block", time.Millisecond)
	BlockValidation("block", 2*time.Millisecond, 3*time.Millisecond)
	BlockValidation("missing", 0, 0)
}
