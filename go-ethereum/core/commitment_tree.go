package core

import (
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/zktx"
)

// transactionCommitments is shared by normal execution and startup recovery.
// The receiver commitment only exists for the two-proof Transfer variant.
// Public/legacy Update transactions do not insert state-tree commitments.
// Execution inserts before ApplyMessage; receipt success is not an additional
// membership condition, so reconstruction must not filter by receipt status.
func transactionCommitments(tx *types.Transaction) []*common.Hash {
	var commitments []*common.Hash
	switch tx.TxCode() {
	case types.MintTx, types.RedeemTx, types.CreateAccountTx, types.TransferTx:
		if tx.ZKCMT() != nil {
			commitments = append(commitments, tx.ZKCMT())
		}
		if tx.TxCode() == types.TransferTx && len(tx.ZKProof2()) > 0 && tx.ZKCMT2() != nil {
			commitments = append(commitments, tx.ZKCMT2())
		}
	}
	return commitments
}

// replayCanonicalCommitments replays only public commitment insertions, never
// ApplyTransaction, private account state, EVM balances, or transaction proofs.
// Blocks are read in canonical height order through the loaded full-state head;
// fast-sync headers, side chains, and pending transactions are not considered.
func replayCanonicalCommitments(head *types.Block, readBlock func(uint64) *types.Block, insert func(*common.Hash) error, interrupted func() bool) (int, error) {
	if head == nil {
		return 0, errors.New("cannot restore commitment tree without a full chain head")
	}
	seen := make(map[common.Hash]struct{})
	var previous common.Hash
	inserted := 0
	for number := uint64(0); ; number++ {
		if interrupted != nil && interrupted() {
			return inserted, errors.New("commitment tree restoration interrupted")
		}
		block := readBlock(number)
		if block == nil || block.NumberU64() != number {
			return inserted, fmt.Errorf("canonical block %d unavailable during commitment tree restoration", number)
		}
		if number > 0 && block.ParentHash() != previous {
			return inserted, fmt.Errorf("canonical chain discontinuity at block %d during commitment tree restoration", number)
		}
		if number == head.NumberU64() && block.Hash() != head.Hash() {
			return inserted, errors.New("canonical head changed during commitment tree restoration")
		}
		for _, tx := range block.Transactions() {
			for _, commitment := range transactionCommitments(tx) {
				if _, exists := seen[*commitment]; exists {
					continue
				}
				if interrupted != nil && interrupted() {
					return inserted, errors.New("commitment tree restoration interrupted")
				}
				if err := insert(commitment); err != nil {
					return inserted, fmt.Errorf("restore commitment tree at block %d: %v", number, err)
				}
				seen[*commitment] = struct{}{}
				inserted++
			}
		}
		previous = block.Hash()
		if number == head.NumberU64() {
			return inserted, nil
		}
	}
}

// rebuildCommitmentTree must run during construction before any chain update,
// mining, txpool, or RPC goroutine can observe the process-wide native tree.
// Live reorg/candidate-block isolation is a separate existing limitation of the
// singleton tree and is not implemented by this startup-only reconstruction.
func (bc *BlockChain) rebuildCommitmentTree() error {
	started := time.Now()
	head := bc.CurrentBlock()
	zktx.ResetSMT()
	count, err := replayCanonicalCommitments(head, bc.GetBlockByNumber, func(commitment *common.Hash) error {
		zktx.InsertCMT(commitment)
		if !zktx.ContainsCMT(commitment) {
			return errors.New("native commitment tree insertion could not be verified")
		}
		return nil
	}, bc.getProcInterrupt)
	if err != nil {
		zktx.ResetSMT()
		return err
	}
	log.Info("Restored canonical commitment tree", "head", head.NumberU64(), "commitments", count, "elapsed", time.Since(started))
	return nil
}
