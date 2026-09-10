package ethapi

import (
	"context"
	"errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"math/big"
	"testing"
)

func TestCommitmentLocationCanonicalCache(t *testing.T) {
	target := common.HexToHash("01")
	blocks := make(map[uint64]*types.Block)
	for i := uint64(0); i < 100; i++ {
		h := &types.Header{Number: new(big.Int).SetUint64(i), Extra: []byte{1}}
		if i == 4 {
			h.CMT = []*common.Hash{&target}
		}
		blocks[i] = types.NewBlockWithHeader(h)
	}
	reads := 0
	read := func(n uint64) (*types.Block, error) { reads++; return blocks[n], nil }
	var cache commitmentLocation
	height, err := cache.locate(context.Background(), blocks[99], target, read)
	if err != nil || height != 4 {
		t.Fatalf("initial lookup %d %v", height, err)
	}
	reads = 0
	height, err = cache.locate(context.Background(), blocks[99], target, read)
	if err != nil || height != 4 || reads != 1 {
		t.Fatalf("cache did not use one canonical check: %d %d %v", height, reads, err)
	}
	// Reorg removes the cached commitment: it must no longer be reported.
	blocks[4] = types.NewBlockWithHeader(&types.Header{Number: big.NewInt(4), Extra: []byte{2}})
	height, err = cache.locate(context.Background(), blocks[99], target, read)
	if err != nil || height != 0 {
		t.Fatalf("stale fork accepted: %d %v", height, err)
	}
	reads = 0
	_, err = cache.locate(context.Background(), blocks[99], target, read)
	if err != nil || reads != 1 {
		t.Fatal("negative cache rescanned unchanged chain")
	}
	// A fork invalidates a negative scan as well as a positive location.
	blocks[99] = types.NewBlockWithHeader(&types.Header{Number: big.NewInt(99), Extra: []byte{3}})
	blocks[40] = types.NewBlockWithHeader(&types.Header{Number: big.NewInt(40), CMT: []*common.Hash{&target}})
	height, err = cache.locate(context.Background(), blocks[99], target, read)
	if err != nil || height != 40 {
		t.Fatalf("negative fork cache hid inclusion: %d %v", height, err)
	}
	// Switch commitment: no result from the preceding account state may leak.
	nextTarget := common.HexToHash("02")
	height, err = cache.locate(context.Background(), blocks[99], nextTarget, read)
	if err != nil || height != 0 {
		t.Fatalf("previous commitment reused: %d %v", height, err)
	}
	blocks[100] = types.NewBlockWithHeader(&types.Header{Number: big.NewInt(100), CMT: []*common.Hash{&nextTarget}})
	reads = 0
	height, err = cache.locate(context.Background(), blocks[100], nextTarget, read)
	if err != nil || height != 100 || reads != 2 {
		t.Fatalf("incremental inclusion %d %d %v", height, reads, err)
	}
}

func TestCommitmentLocationCancellationAndReadFailure(t *testing.T) {
	var cache commitmentLocation
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.locate(ctx, nil, common.Hash{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	head := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(2)})
	_, err := cache.locate(context.Background(), head, common.Hash{}, func(uint64) (*types.Block, error) { return nil, errors.New("read failed") })
	if err == nil || cache.valid {
		t.Fatal("failed read populated cache")
	}
	ctx, cancel = context.WithCancel(context.Background())
	_, err = cache.locate(ctx, head, common.HexToHash("03"), func(n uint64) (*types.Block, error) {
		cancel()
		return types.NewBlockWithHeader(&types.Header{Number: new(big.Int).SetUint64(n)}), nil
	})
	if !errors.Is(err, context.Canceled) || cache.valid {
		t.Fatal("scan ignored cancellation or cached partial result")
	}
}
