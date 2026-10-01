package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pfap/lab/internal/model"
)

const unconfirmedPrivateStateMessage = "已初始化账户未恢复有效的隐私状态或承诺树，已暂停该节点的隐私交易；请检查 SN 读取和承诺树恢复，不要重复 CreateAccount"

// Once the payer is frozen, allow several bounded probes under load before
// declaring an unknown result. This changes admission waiting, never RPC retry.
const receiverAdmissionTimeout = 5 * time.Minute
const receiverAdmissionProbeTimeout = 60 * time.Second

func probeTransferPair(ctx context.Context, timeout time.Duration, probes ...func(context.Context)) {
	var wg sync.WaitGroup
	for _, probe := range probes {
		wg.Add(1)
		go func(probe func(context.Context)) {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if probeCtx.Err() == nil {
				probe(probeCtx)
			}
		}(probe)
	}
	wg.Wait()
}

// Retry only a rejected liveness admission, never an RPC. Also used before
// ordinary transaction submission and payer proof generation. Reservations
// remain owned by the original execution goroutine throughout this wait.
func waitTransferReceiverAdmission(ctx context.Context, interval time.Duration, admit func() error, refresh func(context.Context)) error {
	var lastAdmission error
	deadlineError := func() error {
		if lastAdmission != nil {
			return fmt.Errorf("admission wait ended (last rejection: %v): %w", lastAdmission, ctx.Err())
		}
		return ctx.Err()
	}
	for {
		if err := ctx.Err(); err != nil {
			return deadlineError()
		}
		err := admit()
		if err == nil || err != errNodeUnavailable {
			return err
		}
		lastAdmission = err
		refresh(ctx)
		if err := ctx.Err(); err != nil {
			return deadlineError()
		}
		err = admit()
		if err == nil || err != errNodeUnavailable {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return deadlineError()
		case <-timer.C:
		}
	}
}

func (a *API) beginTransferReceiverRPC(txID, payerCommitment string) error {
	if !transactionHashPattern.MatchString(payerCommitment) {
		return errors.New("付款方证明未返回合法的新承诺，未提交接收方交易")
	}
	return a.beginTransactionRPCWithPayerState(txID, "submit", payerCommitment)
}

// getPayerNextState freezes the current payer before the receiver submits the
// same transfer. A monitor can therefore observe its not-yet-on-chain CMT. This
// narrowly recognizes that exact expected transition, not arbitrary busy-node
// errors. The exception exists only in the validation copy for this one stage;
// it neither clears persisted diagnostics nor admits a second transaction.
func transactionNodesAvailableAfterPayerProof(s model.State, tx model.Transaction, stage, payerCommitment string, now time.Time) error {
	eligible := tx.Type == "transfer" && tx.Status == "proving" && tx.ExecutionStage == "payer-proof" &&
		stage == "submit" && tx.SubmissionAttemptedAt.IsZero() && !tx.ProvingAt.IsZero() &&
		(tx.RunPhase == "warmup" || tx.RunPhase == "measuring") && transactionHashPattern.MatchString(payerCommitment)
	readyPool := false
	if eligible {
		for _, run := range s.Workloads {
			if run.ID == tx.WorkloadID && run.ExperimentID == tx.ExperimentID && run.Strategy == "ready-pool" {
				readyPool = true
				break
			}
		}
	}
	if readyPool {
		for ei, exp := range s.Experiments {
			if exp.ID != tx.ExperimentID {
				continue
			}
			for ni, node := range exp.Nodes {
				stateBlock, knownBlock := parseBlockValue(node.LastTxBlock)
				balance, knownBalance := amount(node.ZKBalance)
				expected, knownExpected := amount(tx.ExpectedPayerBalance)
				if node.ID != tx.FromNode || node.PrivateStateError != unconfirmedPrivateStateMessage ||
					node.RecoveryWarning != "" || !knownBlock || stateBlock != 0 ||
					!strings.EqualFold(node.Commitment, payerCommitment) ||
					!knownBalance || !knownExpected || balance.Cmp(expected) != 0 ||
					node.LastSeen.IsZero() || node.LastSeen.Before(tx.ProvingAt) || node.LastSeen.After(now) || now.Sub(node.LastSeen) > 2*time.Minute {
					continue
				}
				// The normal validator still enforces experiment/node liveness,
				// both miner roles, and every receiver-side privacy check.
				s.Experiments = slices.Clone(s.Experiments)
				s.Experiments[ei].Nodes = slices.Clone(exp.Nodes)
				s.Experiments[ei].Nodes[ni].PrivateStateError = ""
				return transactionNodesAvailable(s, tx)
			}
		}
	}
	return transactionNodesAvailable(s, tx)
}
