package api

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pfap/lab/internal/model"
)

const legacyConsoleStartupFatal = "Fatal: Failed to start the JavaScript console: api modules: context deadline exceeded"
const legacyPublicStartupConclusion = "核验结论：旧版控制台在初始化 API 模块时退出，尚未执行 Public 交易表达式；已标记失败并释放该交易占用，未发送或重发任何交易。"

// This recognizes only the exact old controller/SSH wrapper and Fatalf output.
// remoteConsole calls console.New before Evaluate (cmd/geth/consolecmd.go).
// utils.Fatalf exits with status 1 and prints the same newline-terminated line
// once or twice depending on stdout/stderr identity (cmd/utils/cmd.go).
// Never generalize this to an arbitrary timeout, extra output, or an anonymous
// transaction: a Transfer payer may already have frozen its private state.
func legacyPublicConsoleNeverEvaluated(tx model.Transaction) bool {
	if tx.Type != "public" || tx.Status != "unknown" || tx.ExecutionStage != "submit" || tx.FromNode == "" ||
		tx.Hash != "" || tx.Receipt != "" || !tx.BroadcastAt.IsZero() || !tx.ConfirmedAt.IsZero() || !tx.ReadyAt.IsZero() ||
		tx.BlockNumber != "" || tx.ReceiptStatus != "" || tx.SubmissionAttemptedAt.IsZero() || tx.ProvingAt.IsZero() {
		return false
	}
	const prefix = "执行结果待核验；不会自动重发，相关节点继续保留交易占用。 RPC 返回失败：ssh: exit status 1: "
	for _, lines := range []int{1, 2} {
		output := strings.Repeat(legacyConsoleStartupFatal+"\n", lines)
		if tx.Error == prefix+strings.TrimSuffix(output, "\n")+": "+output {
			return true
		}
	}
	return false
}

// A narrow, explicit reconciliation of persisted non-execution evidence. No
// worker calls are made. The original execution lock must be free, and both
// eligibility and the lock's node identity are checked again at commit time.
func (a *API) reconcileLegacyPublicStartup(ctx context.Context, tx model.Transaction) (model.Transaction, bool, error) {
	if !legacyPublicConsoleNeverEvaluated(tx) {
		return tx, false, nil
	}
	l, _ := a.nodeLocks.LoadOrStore(tx.FromNode, &sync.Mutex{})
	lock := l.(*sync.Mutex)
	if !lock.TryLock() {
		return tx, true, errors.New("原交易执行尚未结束，请稍后核验；交易占用保持不变")
	}
	defer lock.Unlock()
	if _, pending := a.pendingUncertainty.Load(tx.ID); pending {
		return tx, true, errors.New("该交易还有未落库执行结果，请等待存储恢复后核验")
	}
	if _, pending := a.pendingTxFinishes.Load(tx.ID); pending {
		return tx, true, errors.New("该交易还有未落库结束结果，请等待存储恢复后核验")
	}
	var event model.Event
	result := tx
	err := a.store.Update(func(s *model.State) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for i := range s.Transactions {
			current := &s.Transactions[i]
			if current.ID != tx.ID {
				continue
			}
			if current.FromNode != tx.FromNode || current.ExperimentID != tx.ExperimentID || !legacyPublicConsoleNeverEvaluated(*current) {
				return errors.New("交易执行证据已变化，不能确认未执行；请重新核验")
			}
			now := time.Now()
			current.Status = "failed"
			current.LastCheckedAt = now
			current.ReconciliationError = ""
			// Preserve the complete original error; append the separate conclusion.
			current.Error += "\n" + legacyPublicStartupConclusion
			event = model.Event{ID: id("evt"), ExperimentID: current.ExperimentID, Level: "info", Kind: "transaction-reconciled", Message: legacyPublicStartupConclusion, Fields: map[string]any{"id": current.ID, "type": "public", "result": "not-executed", "evidence": "legacy-console-startup-fatal"}, At: now}
			s.Events = append(s.Events, event)
			result = *current
			return nil
		}
		return os.ErrNotExist
	})
	if err != nil {
		return tx, true, err
	}
	// Publish only after the result and its audit event have committed together.
	a.mu.Lock()
	for ch := range a.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
	a.mu.Unlock()
	return result, true, nil
}
