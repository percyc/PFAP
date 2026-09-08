package core

import (
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/zktx"
)

func resetTransferHistory() {
	transferHistory.Lock()
	defer transferHistory.Unlock()
	for _, entry := range transferHistory.snapshots {
		entry.tree.Close()
	}
	transferHistory.snapshots = nil
	transferHistory.roots = make(map[common.Hash]common.Hash)
}

func historyReader(blocks ...*types.Block) CommitmentBlockReader {
	byHash := make(map[common.Hash]*types.Block)
	for _, block := range blocks {
		byHash[block.Hash()] = block
	}
	return func(hash common.Hash, height uint64) *types.Block {
		block := byHash[hash]
		if block != nil && block.NumberU64() == height {
			return block
		}
		return nil
	}
}

func TestTransferHistoryRootAndFork(t *testing.T) {
	resetTransferHistory()
	defer resetTransferHistory()
	genesis := commitmentTestBlock(nil)
	a := commitmentTestBlock(genesis, commitmentTestTransaction(types.CreateAccountTx, 1, 0, false))
	b := commitmentTestBlock(a, commitmentTestTransaction(types.CreateAccountTx, 2, 0, false))
	fork := commitmentTestBlock(genesis, commitmentTestTransaction(types.CreateAccountTx, 3, 0, false))
	read := historyReader(genesis, a, b, fork)
	root, witness, err := CommitmentWitnessAt(a, read, commitmentTestHash(1))
	if err != nil {
		t.Fatal(err)
	}
	latest, _, err := CommitmentWitnessAt(b, read, commitmentTestHash(2))
	if err != nil || root == latest {
		t.Fatalf("root did not advance: %v", err)
	}
	if _, err := ValidateTransferRoot(b, read, []uint64{1}, root); err != nil {
		t.Fatal("historical root rejected:", err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := ValidateTransferRoot(b, read, []uint64{1}, root); err != nil {
				t.Error(err)
			}
			if _, w, err := CommitmentWitnessAt(a, read, commitmentTestHash(1)); err != nil || w != witness {
				t.Error("concurrent witness changed", err)
			}
		}()
	}
	workers.Wait()
	if _, err := ValidateTransferRoot(fork, read, []uint64{1}, root); err == nil {
		t.Fatal("orphan root accepted")
	}
	for _, anchor := range [][]uint64{nil, {1, 2}, {99}} {
		if _, err := ValidateTransferRoot(b, read, anchor, root); err == nil {
			t.Fatal("invalid anchor accepted", anchor)
		}
	}
	if _, err := ValidateTransferRoot(b, read, []uint64{1}, commitmentTestHash(99)); err == nil {
		t.Fatal("fabricated root accepted")
	}
	if _, _, err := CommitmentWitnessAt(a, read, commitmentTestHash(2)); err == nil {
		t.Fatal("future receiver state accepted")
	}
	// Speculative execution must not contaminate historical membership.
	zktx.InsertCMT(ptrHistoryHash(99))
	if _, _, err := CommitmentWitnessAt(b, read, commitmentTestHash(99)); err == nil {
		t.Fatal("global singleton contaminated history")
	}
	_, again, err := CommitmentWitnessAt(a, read, commitmentTestHash(1))
	if err != nil || again != witness {
		t.Fatal("immutable historical witness changed", err)
	}
	// Cold reconstruction after eviction/restart must reproduce exactly the same root.
	resetTransferHistory()
	if _, err := ValidateTransferRoot(b, read, []uint64{1}, root); err != nil {
		t.Fatal(err)
	}
	resetTransferHistory()
	if _, err := ValidateTransferRoot(b, historyReader(b), []uint64{1}, root); err == nil {
		t.Fatal("missing ancestry accepted")
	}
}

func ptrHistoryHash(value byte) *common.Hash { h := commitmentTestHash(value); return &h }

func TestTransferRequiresTwoDistinctStates(t *testing.T) {
	tx := commitmentTestTransaction(types.TransferTx, 1, 2, true)
	if ValidateTransferShape(tx) == nil {
		t.Fatal("incomplete proof accepted")
	}
	tx.SetZKSN(ptrHistoryHash(3))
	tx.SetZKSNS(ptrHistoryHash(4))
	tx.SetZKCMTS(ptrHistoryHash(5))
	tx.SetZKProof(make([]byte, zktx.TransferProofHexSize))
	tx.SetZKProof2(make([]byte, zktx.TransferProofHexSize))
	if err := ValidateTransferShape(tx); err != nil {
		t.Fatal(err)
	}
	if err := validateTransferSerials(tx, func(common.Address) bool { return false }); err != nil {
		t.Fatal(err)
	}
	for _, spent := range []*common.Hash{tx.ZKSN(), tx.ZKSNS()} {
		if validateTransferSerials(tx, func(address common.Address) bool { return address == common.BytesToAddress(spent.Bytes()) }) == nil {
			t.Fatal("spent payer/receiver state accepted")
		}
	}
	tx.SetZKSNS(tx.ZKSN())
	if ValidateTransferShape(tx) == nil {
		t.Fatal("same account accepted")
	}
}

func BenchmarkTransferHistoryCachedRoot(b *testing.B) {
	resetTransferHistory()
	defer resetTransferHistory()
	genesis := commitmentTestBlock(nil)
	anchor := commitmentTestBlock(genesis, commitmentTestTransaction(types.CreateAccountTx, 1, 0, false))
	read := historyReader(genesis, anchor)
	root, _, err := CommitmentWitnessAt(anchor, read, commitmentTestHash(1))
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ValidateTransferRoot(anchor, read, []uint64{1}, root); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTransferHistory120Ancestors(b *testing.B) {
	resetTransferHistory()
	defer resetTransferHistory()
	genesis := commitmentTestBlock(nil)
	anchor := commitmentTestBlock(genesis, commitmentTestTransaction(types.CreateAccountTx, 1, 0, false))
	blocks := []*types.Block{genesis, anchor}
	for i := 0; i < 120; i++ {
		blocks = append(blocks, commitmentTestBlock(blocks[len(blocks)-1]))
	}
	read := historyReader(blocks...)
	root, _, err := CommitmentWitnessAt(anchor, read, commitmentTestHash(1))
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ValidateTransferRoot(blocks[len(blocks)-1], read, []uint64{1}, root); err != nil {
			b.Fatal(err)
		}
	}
}
