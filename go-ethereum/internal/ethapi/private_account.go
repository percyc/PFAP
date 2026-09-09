package ethapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/zktx"
)

func (s *PublicTransactionPoolAPI) beginPrivateMutation(ctx context.Context, create bool) (func(), error) {
	if !zktx.AccountMu.TryLock() {
		return nil, errors.New("private account busy; one in-flight operation per account")
	}
	if err := zktx.PrivateAccountWritable(create); err != nil {
		zktx.AccountMu.Unlock()
		return nil, err
	}
	if !create {
		// Canonical block-boundary membership, not the mutable miner singleton.
		// A pending payer/receiver state cannot be used for a second transaction.
		if _, _, err := core.CommitmentWitnessAt(s.b.CurrentBlock(), s.commitmentBlockReader(ctx), *zktx.SequenceNumberAfter.CMT); err != nil {
			zktx.AccountMu.Unlock()
			return nil, fmt.Errorf("private account pending canonical confirmation; do not replay or reset: %v", err)
		}
	}
	return zktx.AccountMu.Unlock, nil
}

func (s *PublicTransactionPoolAPI) persistAndSubmitPrivate(ctx context.Context, signed *types.Transaction, next zktx.SequenceS, key *common.Hash) (common.Hash, error) {
	if err := ctx.Err(); err != nil {
		return common.Hash{}, err
	}
	raw, err := rlp.EncodeToBytes(signed)
	if err != nil {
		return common.Hash{}, err
	}
	if err := zktx.PersistPrivateTransition(next, key, "transaction", signed.Hash(), raw); err != nil {
		return common.Hash{}, err
	}
	hash, err := submitTransaction(ctx, s.b, signed)
	if err != nil {
		return signed.Hash(), fmt.Errorf("durable private transaction %s requires reconciliation; no automatic replay: %v", signed.Hash().Hex(), err)
	}
	return hash, nil
}

// Read-only recovery metadata for a lost RPC response. It does not broadcast,
// clear a reservation, disclose the secret, or return the signed payload.
func (s *PublicTransactionPoolAPI) GetPrivateRecovery(ctx context.Context) (map[string]interface{}, error) {
	zktx.AccountMu.Lock()
	defer zktx.AccountMu.Unlock()
	kind, hash, problem := zktx.PrivateAccountRecoveryInfo()
	return map[string]interface{}{"kind": kind, "transactionHash": hash, "error": problem}, nil
}
