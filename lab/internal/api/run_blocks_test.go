package api

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func TestRunBlockExpressionAnchorsFirstSampleToSingleHeadRead(t *testing.T) {
	expr := runBlockExpression(nil, 2)
	if strings.Count(expr, "eth.blockNumber") != 1 {
		t.Fatal("first observer sample must not read a potentially advancing head twice")
	}
	if !strings.Contains(expr, "depth=2;") || !strings.Contains(expr, "cutoff=h-depth+1,start=cutoff,") {
		t.Fatal("first observer sample must use the captured head minus confirmation depth")
	}
	if !strings.Contains(expr, "eth.getBlock(b.parentHash)") || !strings.Contains(expr, "canonical.hash!==anchor.hash") || !strings.Contains(expr, "out.reverse()") {
		t.Fatal("observer must follow and revalidate one immutable parent-hash branch")
	}
}

func TestRunBlockExpressionRechecksLastObservedBlock(t *testing.T) {
	expr := runBlockExpression([]model.RunBlock{{Number: 41}, {Number: 42}}, 2)
	if strings.Count(expr, "eth.blockNumber") != 1 || !strings.Contains(expr, "start=42,") {
		t.Fatal("subsequent observer samples must start at the last observed block for reorg detection")
	}
	if !strings.Contains(expr, "if(cutoff-start>64)") || strings.Contains(expr, "eth.getBlock(i)") {
		t.Fatal("observer lost the backlog bound or resumed per-height mixed-branch reads")
	}
}

func testObservedBlock(number uint64, hash, parent string) model.RunBlock {
	return model.RunBlock{Number: number, Hash: hash, ParentHash: parent, Timestamp: number, Size: 100}
}

func TestRunBlockBatchPreservesAnchorObservationTimeAndDepth(t *testing.T) {
	now := time.Now()
	w := model.Workload{ID: "w", Status: "running", Phase: "warmup", Confirmations: 2}
	s := model.State{}
	first := testObservedBlock(41, "a41", "a40")
	if applyRunBlockBatch(&s, &w, runBlockBatch{Head: 42, Cutoff: 41, Blocks: []model.RunBlock{first}}, "", now) {
		t.Fatal(w.BlockError)
	}
	w.Phase, w.MeasurementStartedAt = "measuring", now.Add(time.Second)
	batch := runBlockBatch{Head: 43, Cutoff: 42, Blocks: []model.RunBlock{first, testObservedBlock(42, "a42", "a41")}}
	if applyRunBlockBatch(&s, &w, batch, "", now.Add(2*time.Second)) || len(w.Blocks) != 2 {
		t.Fatal(w.BlockError)
	}
	if !w.Blocks[0].ObservedAt.Equal(now) || w.Configuration["blockSampling"] != confirmedBlockSampling || w.Configuration["blockSamplingConfirmations"] != 2 {
		t.Fatal("duplicate anchor was re-timestamped or sampling version was lost")
	}
	report := blockRunReport(w, now.Add(3*time.Second))
	if report["count"] != 1 || !strings.Contains(report["scope"].(string), "确认等待") {
		t.Fatal("warmup anchor leaked into the formal window or delay was not disclosed")
	}
}

func TestRunBlockBatchWarmupReorgResetsWithAuditAndWait(t *testing.T) {
	now := time.Now()
	w := model.Workload{ID: "w", ExperimentID: "e", Status: "running", Phase: "warmup", Confirmations: 2, WarmupSeconds: 30, StartedAt: now.Add(-time.Minute),
		Configuration: map[string]any{"blockSampling": confirmedBlockSampling}, Blocks: []model.RunBlock{testObservedBlock(554, "a554", "a553"), testObservedBlock(555, "old555", "a554")}}
	s := model.State{}
	batch := runBlockBatch{Head: 557, Cutoff: 556, Blocks: []model.RunBlock{testObservedBlock(555, "new555", "a554"), testObservedBlock(556, "new556", "new555")}}
	if applyRunBlockBatch(&s, &w, batch, "", now) || w.BlockError != "" || len(w.Blocks) != 2 || w.Blocks[0].Hash != "new555" {
		t.Fatal("warmup did not rebuild from the new confirmed branch")
	}
	if len(s.Events) != 1 || s.Events[0].Kind != "block-observer-reorg" || s.Events[0].Fields["previousAnchor"].(model.RunBlock).Hash != "old555" || runWarmupBlockReorgs(w) != 1 {
		t.Fatal("warmup reorg evidence was discarded")
	}
	if runBlockWarmupReady(w, now.Add(29*time.Second)) || !runBlockWarmupReady(w, now.Add(30*time.Second)) || !runBlockWarmupStart(w).Equal(now) {
		t.Fatal("warmup did not wait its full configured duration after the reorg")
	}
}

func TestRunBlockBatchFormalReorgCannotRewriteWindow(t *testing.T) {
	for _, phase := range []string{"warmup", "measuring", "draining"} {
		t.Run(phase, func(t *testing.T) {
			now := time.Now()
			old := testObservedBlock(555, "old555", "a554")
			w := model.Workload{ID: "w", Status: "running", Phase: phase, Confirmations: 2, MeasurementStartedAt: now.Add(-time.Minute), Blocks: []model.RunBlock{old}}
			s := model.State{}
			batch := runBlockBatch{Head: 557, Cutoff: 556, Blocks: []model.RunBlock{testObservedBlock(555, "new555", "a554"), testObservedBlock(556, "new556", "new555")}}
			if !applyRunBlockBatch(&s, &w, batch, "", now) || w.BlockError == "" || !reflect.DeepEqual(w.Blocks, []model.RunBlock{old}) || len(s.Events) != 1 {
				t.Fatal("measured reorg was ignored or the original window was rewritten")
			}
		})
	}
}

func TestRunBlockBatchPreservesFirstErrorAndTerminalResults(t *testing.T) {
	for _, status := range []string{"draining", "completed", "interrupted"} {
		w := model.Workload{Status: status, BlockError: "first reorg evidence", Blocks: []model.RunBlock{testObservedBlock(555, "old", "parent")}}
		before := w
		s := model.State{}
		if !applyRunBlockBatch(&s, &w, runBlockBatch{}, "later query failure", time.Now()) || !reflect.DeepEqual(before, w) || len(s.Events) != 0 {
			t.Fatal("late collector error overwrote the first error or historical result")
		}
	}
	w := model.Workload{Status: "completed"}
	if !applyRunBlockBatch(&model.State{}, &w, runBlockBatch{}, "late failure", time.Now()) || w.BlockError != "" {
		t.Fatal("late query changed a completed result")
	}
}

func TestRunBlockBatchRejectsPartialAndImmatureSamples(t *testing.T) {
	for _, tc := range []struct {
		name  string
		batch runBlockBatch
	}{
		{"empty", runBlockBatch{}},
		{"immature head", runBlockBatch{Head: 41, Cutoff: 41, Blocks: []model.RunBlock{testObservedBlock(41, "a41", "a40")}}},
		{"wrong cutoff", runBlockBatch{Head: 44, Cutoff: 42, Blocks: []model.RunBlock{testObservedBlock(42, "a42", "a41")}}},
		{"gap", runBlockBatch{Head: 44, Cutoff: 43, Blocks: []model.RunBlock{testObservedBlock(41, "a41", "a40"), testObservedBlock(43, "a43", "a41")}}},
		{"mixed branch", runBlockBatch{Head: 44, Cutoff: 43, Blocks: []model.RunBlock{testObservedBlock(41, "a41", "a40"), testObservedBlock(42, "a42", "a41"), testObservedBlock(43, "b43", "b42")}}},
		{"missed cursor", runBlockBatch{Head: 44, Cutoff: 43, Blocks: []model.RunBlock{testObservedBlock(42, "a42", "a41"), testObservedBlock(43, "a43", "a42")}}},
		{"unstable", runBlockBatch{Unstable: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := testObservedBlock(41, "a41", "a40")
			w := model.Workload{Status: "running", Phase: "warmup", Confirmations: 2, Blocks: []model.RunBlock{old}}
			if !applyRunBlockBatch(&model.State{}, &w, tc.batch, "", time.Now()) || w.BlockError == "" || !reflect.DeepEqual(w.Blocks, []model.RunBlock{old}) {
				t.Fatal("invalid batch was partially admitted")
			}
		})
	}
}

func TestRunBlockBatchWaitsForInitialConfirmationDepth(t *testing.T) {
	w := model.Workload{Status: "running", Phase: "warmup", Confirmations: 3}
	if applyRunBlockBatch(&model.State{}, &w, runBlockBatch{Head: 1, Waiting: true}, "", time.Now()) || w.BlockError != "" || len(w.Blocks) != 0 {
		t.Fatal("initial insufficient chain depth was marked as a failure")
	}
}

func TestRunBlockWarmupAndReportRetainLegacySemantics(t *testing.T) {
	now := time.Now()
	w := model.Workload{StartedAt: now.Add(-time.Minute), WarmupSeconds: 30, Confirmations: 2}
	if !runBlockWarmupReady(w, now) || blockRunReport(w, now)["samplingVersion"] != "head-v1" {
		t.Fatal("legacy warmup or report was reinterpreted")
	}
	w.StartedAt = now
	if runBlockWarmupReady(w, now.Add(29*time.Second)) {
		t.Fatal("unexpired original warmup was considered ready")
	}
}
