package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/orchestrator"
)

const confirmedBlockSampling = "confirmed-parent-hash-v2"

type runBlockBatch struct {
	Head     uint64           `json:"head"`
	Cutoff   uint64           `json:"cutoff"`
	Blocks   []model.RunBlock `json:"blocks"`
	Waiting  bool             `json:"waiting"`
	Unstable bool             `json:"unstable"`
}

func runBlockDepth(w model.Workload) int { return max(1, w.Confirmations) }

func runBlockExpression(blocks []model.RunBlock, confirmations int) string {
	// Capture one mature anchor, then follow immutable parent hashes. Reading
	// every height independently can splice two branches during a reorg.
	cursor := "cutoff"
	if len(blocks) > 0 {
		cursor = strconv.FormatUint(blocks[len(blocks)-1].Number, 10)
	}
	return `(function(){var h=eth.blockNumber,depth=` + strconv.Itoa(max(1, confirmations)) + `;if(h<depth-1)return JSON.stringify({head:h,waiting:true,blocks:[]});var cutoff=h-depth+1,start=` + cursor + `,out=[];if(cutoff-start>64)throw new Error("observer backlog exceeds 64 blocks");if(start>cutoff)start=cutoff;var anchor=eth.getBlock(cutoff),b=anchor;if(!b)throw new Error("missing mature anchor");while(true){out.push({number:b.number,hash:b.hash,parentHash:b.parentHash,timestamp:b.timestamp,size:Number(b.size),transactions:b.transactions.length});if(b.number===start)break;if(b.number<start||out.length>64)throw new Error("invalid parent chain");var p=eth.getBlock(b.parentHash);if(!p||p.hash!==b.parentHash||p.number+1!==b.number)throw new Error("missing parent block");b=p;}var canonical=eth.getBlock(cutoff);if(!canonical||canonical.hash!==anchor.hash)return JSON.stringify({head:h,cutoff:cutoff,unstable:true,blocks:[]});return JSON.stringify({head:h,cutoff:cutoff,blocks:out.reverse()})})()`
}

func runBlockWarmupStart(w model.Workload) time.Time {
	start := w.StartedAt
	if text, ok := w.Configuration["blockWarmupResetAt"].(string); ok {
		if reset, err := time.Parse(time.RFC3339Nano, text); err == nil && reset.After(start) {
			start = reset
		}
	}
	return start
}

func runBlockWarmupReady(w model.Workload, now time.Time) bool {
	start := runBlockWarmupStart(w)
	return !start.IsZero() && now.Sub(start) >= time.Duration(w.WarmupSeconds)*time.Second
}

func runWarmupBlockReorgs(w model.Workload) int {
	switch n := w.Configuration["blockWarmupReorgs"].(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// Apply a complete, coherent sample atomically. Existing errors and terminal
// results are immutable. Warmup reorgs retain an audit trail and reset warmup;
// a reorg of a measured mature anchor invalidates the window instead.
func applyRunBlockBatch(s *model.State, w *model.Workload, batch runBlockBatch, message string, now time.Time) bool {
	if !runActive(*w) || w.BlockError != "" {
		return true
	}
	if message != "" {
		w.BlockError = message
		return true
	}
	if batch.Unstable {
		w.BlockError = "成熟区块采样期间规范锚持续变化，无法取得一致快照"
		return true
	}
	depth := runBlockDepth(*w)
	if batch.Waiting {
		if len(batch.Blocks) == 0 && batch.Head < uint64(depth-1) && len(w.Blocks) == 0 {
			return false
		}
		w.BlockError = "区块观察返回了不一致的确认深度"
		return true
	}
	if len(batch.Blocks) == 0 || len(batch.Blocks) > 65 || batch.Head < uint64(depth-1) || batch.Cutoff != batch.Head-uint64(depth-1) || batch.Blocks[len(batch.Blocks)-1].Number != batch.Cutoff {
		w.BlockError = "区块观察返回为空或确认深度不一致"
		return true
	}
	for i, b := range batch.Blocks {
		if b.Hash == "" || b.ParentHash == "" || b.Size == 0 || (i > 0 && (b.Number != batch.Blocks[i-1].Number+1 || b.ParentHash != batch.Blocks[i-1].Hash)) {
			w.BlockError = "区块元数据不完整或父哈希链不连续"
			return true
		}
	}
	if w.Configuration == nil {
		w.Configuration = map[string]any{}
	}
	if len(w.Blocks) == 0 {
		w.Configuration["blockSampling"] = confirmedBlockSampling
		w.Configuration["blockSamplingConfirmations"] = depth
	}
	first := batch.Blocks[0]
	appendFrom := 0
	if len(w.Blocks) > 0 {
		last := w.Blocks[len(w.Blocks)-1]
		if first.Number > last.Number {
			w.BlockError = "成熟区块采样遗漏前一锚点，当前窗口无效"
			return true
		}
		if first.Number != last.Number || first.Hash != last.Hash {
			warmup := w.Phase == "warmup" && w.MeasurementStartedAt.IsZero()
			s.Events = append(s.Events, model.Event{ID: id("evt"), ExperimentID: w.ExperimentID, Level: "warn", Kind: "block-observer-reorg", At: now,
				Message: "观察到已达确认深度的规范锚改变；预热重建缓存，正式测量则判定无效。",
				Fields:  map[string]any{"workloadId": w.ID, "phase": w.Phase, "confirmations": depth, "previousAnchor": last, "replacementAnchor": first, "previousCachedBlocks": len(w.Blocks)}})
			if !warmup {
				w.BlockError = "观察到已确认深度区块重组，当前测量窗口无效"
				return true
			}
			w.Configuration["blockWarmupReorgs"] = runWarmupBlockReorgs(*w) + 1
			w.Configuration["blockWarmupResetAt"] = now.Format(time.RFC3339Nano)
			w.Blocks = nil
		} else {
			appendFrom = 1
		}
	}
	for _, b := range batch.Blocks[appendFrom:] {
		b.ObservedAt = now
		w.Blocks = append(w.Blocks, b)
	}
	return false
}

// A dedicated read-only observer. All blocks are labelled by controller
// observation time; batching delay is deliberately not called network latency.
func (a *API) collectRunBlocks(runID string, stop <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		var run model.Workload
		var exp model.Experiment
		var node model.Node
		var server model.Server
		a.store.View(func(s model.State) {
			for _, w := range s.Workloads {
				if w.ID == runID {
					run = w
				}
			}
			for _, e := range s.Experiments {
				if e.ID == run.ExperimentID {
					exp = e
				}
			}
			for _, n := range exp.Nodes {
				if n.ID == run.ObserverNodeID {
					node = n
				}
			}
			for _, srv := range s.Servers {
				if srv.ID == node.ServerID {
					server = srv
				}
			}
		})
		if !runActive(run) || run.BlockError != "" {
			return
		}
		expr := runBlockExpression(run.Blocks, runBlockDepth(run))
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		var batch runBlockBatch
		var err error
		// A changing anchor can be a race between read-only calls. Retry the
		// whole snapshot (never a transaction), bounded by the same deadline.
		for attempt := 0; attempt < 3; attempt++ {
			var out, raw string
			out, err = a.orch.Attach(ctx, exp, node, server, expr)
			if err == nil {
				raw, err = orchestrator.ExtractJSONString(out)
			}
			if err == nil {
				batch = runBlockBatch{}
				err = json.Unmarshal([]byte(raw), &batch)
			}
			if err != nil || !batch.Unstable {
				break
			}
		}
		if err == nil && !batch.Unstable && len(batch.Blocks) > 0 {
			timings, _ := a.orch.BlockValidationTimings(ctx, exp, node, server)
			for i := range batch.Blocks {
				if duration, ok := timings[strings.ToLower(batch.Blocks[i].Hash)]; ok {
					v := duration
					batch.Blocks[i].ValidationUs = &v
				}
			}
		}
		cancel()
		message := ""
		if err != nil {
			message = "区块采集失败，窗口数据不完整"
		}
		done := false
		if err := a.store.Update(func(s *model.State) error {
			for wi := range s.Workloads {
				w := &s.Workloads[wi]
				if w.ID != runID {
					continue
				}
				done = applyRunBlockBatch(s, w, batch, message, time.Now())
				return nil
			}
			return errors.New("自动运行不存在")
		}); err != nil {
			// A collector that silently disappears must not leave a run able to
			// claim a complete window from only the earlier blocks. Existing
			// transaction reservations remain intact while admission is stopped.
			_ = a.finishWorkload(runID, "interrupted", "区块采样保存失败，无法保证窗口完整性")
			return
		}
		if done {
			return
		}
	}
}

func blockRunReport(w model.Workload, end time.Time) map[string]any {
	count, nonempty, validated := 0, 0, 0
	var size, nonemptySize uint64
	var validation int64
	var first, last uint64
	for _, b := range w.Blocks {
		if w.MeasurementStartedAt.IsZero() || b.ObservedAt.Before(w.MeasurementStartedAt) || !b.ObservedAt.Before(end) {
			continue
		}
		if count == 0 {
			first = b.Timestamp
		}
		last = b.Timestamp
		count++
		size += b.Size
		if b.Transactions > 0 {
			nonempty++
			nonemptySize += b.Size
		}
		if b.ValidationUs != nil {
			validated++
			validation += *b.ValidationUs
		}
	}
	var avg, avgNonempty, interval, verify any
	if count > 0 {
		avg = float64(size) / float64(count)
	}
	if nonempty > 0 {
		avgNonempty = float64(nonemptySize) / float64(nonempty)
	}
	if count > 1 && last >= first {
		interval = float64(last-first) / float64(count-1)
	}
	if validated > 0 {
		verify = float64(validation) / float64(validated)
	}
	scope := "按控制器每 5 秒采集到的规范链区块计数；边界有采样误差。间隔使用这些连续区块的区块头时间戳。重组或漏采将标记无效；非最终性保证。"
	version := "head-v1"
	if w.Configuration["blockSampling"] == confirmedBlockSampling {
		version = confirmedBlockSampling
		scope = fmt.Sprintf("仅采集深度至少 %d（含区块自身）的规范链区块，按控制器完成采样时刻归入窗口。存在确认等待、5 秒轮询及批量查询延迟，不等同于实时头或出块时刻；间隔使用连续区块头时间戳。预热成熟锚重组保留审计并重新预热；正式窗口成熟锚重组或缺块判无效。未成熟分叉不计入本指标，深度确认不保证最终性。", runBlockDepth(w))
	}
	return map[string]any{"observerNodeId": w.ObserverNodeID, "count": count, "meanSizeB": avg, "meanNonemptySizeB": avgNonempty, "meanBlockIntervalSeconds": interval, "meanExecutionValidationUs": verify, "validationSamples": validated, "missingValidationSamples": count - validated, "error": w.BlockError, "scope": scope, "samplingVersion": version, "warmupReorgs": runWarmupBlockReorgs(w)}
}
