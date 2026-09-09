package ethapi

import (
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/zktx"
)

type privateSubmitBackend struct {
	Backend
	send func(*types.Transaction) error
}

func (b privateSubmitBackend) SendTx(_ context.Context, tx *types.Transaction) error {
	return b.send(tx)
}

func TestPrivateWriteAheadBeforeBroadcastAndRestart(t *testing.T) {
	zktx.AccountMu.Lock()
	defer zktx.AccountMu.Unlock()
	dir := t.TempDir()
	if err := zktx.RestorePrivateAccount(dir, false); err != nil {
		t.Fatal(err)
	}
	key := common.HexToHash("1234")
	sn := common.HexToHash("1")
	cmt := common.HexToHash("2")
	random := common.HexToHash("3")
	next := zktx.SequenceS{Suquence1: zktx.Sequence{SN: &sn, CMT: &cmt, Random: &random, Value: 10}, Suquence2: zktx.Sequence{SN: &sn, CMT: &cmt, Random: &random, Value: 11}, Stage: zktx.Transfer}
	tx := types.NewTransaction(1, common.Address{}, big.NewInt(0), 21000, big.NewInt(0), nil)
	called := 0
	a := PublicTransactionPoolAPI{b: privateSubmitBackend{send: func(sent *types.Transaction) error {
		called++
		// The broadcast boundary sees durable state, not a deferred write.
		if err := zktx.RestorePrivateAccount(dir, false); err != nil {
			t.Fatal(err)
		}
		kind, hash, problem := zktx.PrivateAccountRecoveryInfo()
		if kind != "transaction" || hash != sent.Hash() || problem != "" || *zktx.AccountSK != key || zktx.SequenceNumberAfter.Value != 11 {
			t.Fatal("broadcast preceded durable state")
		}
		return errors.New("ambiguous submission")
	}}}
	hash, err := a.persistAndSubmitPrivate(context.Background(), tx, next, &key)
	if err == nil || hash != tx.Hash() || called != 1 {
		t.Fatal("submission uncertainty lost")
	}
	if err := zktx.RestorePrivateAccount(dir, false); err != nil {
		t.Fatal(err)
	}
	_, savedHash, _ := zktx.PrivateAccountRecoveryInfo()
	if savedHash != tx.Hash() || zktx.SequenceNumberAfter.Value != 11 {
		t.Fatal("restart lost pending state")
	}
	if err := zktx.PrivateAccountWritable(true); err == nil {
		t.Fatal("pending create could be overwritten")
	}

	badDir := filepath.Join(t.TempDir(), "missing")
	if err := zktx.RestorePrivateAccount(badDir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.persistAndSubmitPrivate(context.Background(), tx, next, &key); err == nil || called != 1 {
		t.Fatal("broadcast after persistence failure")
	}
	if err := zktx.PrivateAccountWritable(false); err == nil {
		t.Fatal("write failure did not fail closed")
	}
	if _, err := os.Stat(filepath.Join(badDir, "private-account-v1.json")); !os.IsNotExist(err) {
		t.Fatal("unexpected durable state")
	}
}

func TestPrivateMutationRejectsConcurrentAccount(t *testing.T) {
	zktx.AccountMu.Lock()
	defer zktx.AccountMu.Unlock()
	a := PublicTransactionPoolAPI{}
	if unlock, err := a.beginPrivateMutation(context.Background(), false); err == nil || unlock != nil {
		t.Fatal("concurrent mutation admitted")
	}
}
