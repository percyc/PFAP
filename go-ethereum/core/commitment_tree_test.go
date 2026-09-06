package core

import (
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/zktx"
)

func commitmentTestHash(value byte) common.Hash { return common.BytesToHash([]byte{value}) }

func commitmentTestTransaction(code uint8, first, second byte, receiverProof bool) *types.Transaction {
	tx := types.NewTransaction(0, common.Address{}, big.NewInt(0), 21000, big.NewInt(0), nil)
	tx.SetTxCode(code)
	a, b := commitmentTestHash(first), commitmentTestHash(second)
	tx.SetZKCMT(&a)
	tx.SetZKCMT2(&b)
	if receiverProof {
		tx.SetZKProof2([]byte{1})
	}
	return tx
}

func commitmentTestBlock(parent *types.Block, txs ...*types.Transaction) *types.Block {
	header := &types.Header{Number: big.NewInt(0), Difficulty: big.NewInt(1), GasLimit: 1000000}
	if parent != nil {
		header.Number.SetUint64(parent.NumberU64() + 1)
		header.ParentHash = parent.Hash()
		header.Root = parent.Root()
	}
	return types.NewBlock(header, txs, nil, nil)
}

func commitmentTestChain(genesis *types.Block) []*types.Block {
	first := commitmentTestBlock(genesis,
		commitmentTestTransaction(types.CreateAccountTx, 1, 0, false),
		commitmentTestTransaction(types.PublicTx, 9, 0, false))
	second := commitmentTestBlock(first,
		commitmentTestTransaction(types.MintTx, 2, 0, false),
		commitmentTestTransaction(types.RedeemTx, 3, 0, false),
		commitmentTestTransaction(types.TransferTx, 4, 5, true),
		commitmentTestTransaction(types.TransferTx, 6, 7, false),
		commitmentTestTransaction(types.CreateAccountTx, 1, 0, false))
	return []*types.Block{genesis, first, second}
}

func TestTransactionCommitmentsMatchesExecutionRules(t *testing.T) {
	for _, tc := range []struct {
		name          string
		code          uint8
		receiverProof bool
		nilFirst      bool
		nilSecond     bool
		want          []byte
	}{
		{name: "CreateAccount", code: types.CreateAccountTx, want: []byte{1}},
		{name: "Mint", code: types.MintTx, want: []byte{1}},
		{name: "Redeem", code: types.RedeemTx, want: []byte{1}},
		{name: "Transfer two proofs", code: types.TransferTx, receiverProof: true, want: []byte{1, 2}},
		{name: "Transfer payer only", code: types.TransferTx, want: []byte{1}},
		{name: "Transfer absent receiver commitment", code: types.TransferTx, receiverProof: true, nilSecond: true, want: []byte{1}},
		{name: "Transfer absent payer commitment", code: types.TransferTx, receiverProof: true, nilFirst: true, want: []byte{2}},
		{name: "Missing commitment", code: types.CreateAccountTx, nilFirst: true},
		{name: "Public ignored", code: types.PublicTx, receiverProof: true},
		{name: "Legacy Update ignored", code: types.UpdateTx, receiverProof: true},
		{name: "Unknown ignored", code: 255, receiverProof: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := commitmentTestTransaction(tc.code, 1, 2, tc.receiverProof)
			if tc.nilFirst {
				tx.SetZKCMT(nil)
			}
			if tc.nilSecond {
				tx.SetZKCMT2(nil)
			}
			got := transactionCommitments(tx)
			if len(got) != len(tc.want) {
				t.Fatalf("commitment count=%d want=%d", len(got), len(tc.want))
			}
			for i, expected := range tc.want {
				if *got[i] != commitmentTestHash(expected) {
					t.Fatalf("incorrect commitment at position %d", i)
				}
			}
		})
	}
}

func TestReplayCanonicalCommitmentsOrderDedupAndHeadBound(t *testing.T) {
	blocks := commitmentTestChain(commitmentTestBlock(nil))
	// A later block must not be replayed past the full-state head.
	blocks = append(blocks, commitmentTestBlock(blocks[2], commitmentTestTransaction(types.CreateAccountTx, 8, 0, false)))
	var inserted []common.Hash
	var heights []uint64
	count, err := replayCanonicalCommitments(blocks[2], func(height uint64) *types.Block {
		heights = append(heights, height)
		return blocks[height]
	}, func(cmt *common.Hash) error { inserted = append(inserted, *cmt); return nil }, nil)
	if err != nil || count != 6 || !reflect.DeepEqual(heights, []uint64{0, 1, 2}) {
		t.Fatalf("canonical replay count=%d heights=%v err=%v", count, heights, err)
	}
	for i, cmt := range inserted {
		if cmt != commitmentTestHash(byte(i+1)) {
			t.Fatalf("canonical transaction/receiver ordering changed at insertion %d", i)
		}
	}
}

func TestReplayCanonicalCommitmentsFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing block", "wrong height", "wrong parent", "different head", "insertion error", "interrupted", "missing head"} {
		t.Run(mode, func(t *testing.T) {
			blocks := commitmentTestChain(commitmentTestBlock(nil))
			head := blocks[2]
			if mode == "missing head" {
				head = nil
			}
			reads, inserts := 0, 0
			_, err := replayCanonicalCommitments(head, func(height uint64) *types.Block {
				reads++
				if height == 1 {
					switch mode {
					case "missing block":
						return nil
					case "wrong height":
						return blocks[2]
					case "wrong parent":
						header := blocks[1].Header()
						header.ParentHash = commitmentTestHash(99)
						return types.NewBlock(header, blocks[1].Transactions(), nil, nil)
					}
				}
				if height == 2 && mode == "different head" {
					header := blocks[2].Header()
					header.Extra = []byte("different canonical block")
					return types.NewBlock(header, blocks[2].Transactions(), nil, nil)
				}
				return blocks[height]
			}, func(*common.Hash) error {
				inserts++
				if mode == "insertion error" {
					return errors.New("native insertion failed")
				}
				return nil
			}, func() bool { return mode == "interrupted" })
			if err == nil {
				t.Fatal("incomplete or corrupt reconstruction reported success")
			}
			if (mode == "interrupted" || mode == "missing head") && (reads != 0 || inserts != 0) {
				t.Fatal("replay continued after interruption or absent head")
			}
			if mode == "insertion error" && (inserts != 1 || !strings.Contains(err.Error(), "native insertion failed")) {
				t.Fatal("insertion failure was swallowed or processing continued")
			}
		})
	}
}

func TestBlockChainStartupRestoresNativeCommitmentTree(t *testing.T) {
	// Populate a synthetic canonical database directly: this tests startup
	// reconstruction, not proof validation or transaction/account execution.
	db := ethdb.NewMemDatabase()
	defer db.Close()
	genesis := (&Genesis{Config: params.AllEthashProtocolChanges}).MustCommit(db)
	blocks := commitmentTestChain(genesis)
	for _, block := range blocks[1:] {
		rawdb.WriteBlock(db, block)
		rawdb.WriteCanonicalHash(db, block.Hash(), block.NumberU64())
		rawdb.WriteTd(db, block.Hash(), block.NumberU64(), big.NewInt(int64(block.NumberU64()+1)))
	}
	head := blocks[len(blocks)-1]
	rawdb.WriteHeadBlockHash(db, head.Hash())
	rawdb.WriteHeadHeaderHash(db, head.Hash())
	rawdb.WriteHeadFastBlockHash(db, head.Hash())
	// A known side block and a stale in-memory commitment must not survive.
	side := commitmentTestBlock(blocks[1], commitmentTestTransaction(types.CreateAccountTx, 8, 0, false))
	rawdb.WriteBlock(db, side)
	stale := commitmentTestHash(10)
	zktx.ResetSMT()
	defer zktx.ResetSMT()
	zktx.InsertCMT(&stale)
	var restoredRoot common.Hash
	for attempt := 0; attempt < 2; attempt++ {
		chain, err := NewBlockChain(db, nil, params.AllEthashProtocolChanges, ethash.NewFaker(), vm.Config{})
		if err != nil {
			t.Fatalf("startup restoration failed: %v", err)
		}
		for value := byte(1); value <= 6; value++ {
			cmt := commitmentTestHash(value)
			if !zktx.ContainsCMT(&cmt) {
				chain.Stop()
				t.Fatalf("canonical commitment %d absent after startup", value)
			}
		}
		for _, value := range []byte{7, 8, 9, 10} {
			cmt := commitmentTestHash(value)
			if zktx.ContainsCMT(&cmt) {
				chain.Stop()
				t.Fatalf("unconfirmed/nonexistent commitment %d leaked into restored tree", value)
			}
		}
		if chain.CurrentBlock().Hash() != head.Hash() || chain.CurrentBlock().Root() != genesis.Root() {
			chain.Stop()
			t.Fatal("restoration changed canonical head or account state root")
		}
		if attempt == 0 {
			restoredRoot = zktx.GetSMTRoot()
		} else if zktx.GetSMTRoot() != restoredRoot {
			chain.Stop()
			t.Fatal("repeated startup produced a different commitment root")
		}
		chain.Stop()
		zktx.ResetSMT()
	}
}
