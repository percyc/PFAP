package ethapi

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Access is serialized by AccountMu. Cache entries are hints, never authority:
// canonical hashes are rechecked before reusing either a hit or a scanned prefix.
type commitmentLocation struct {
	commitment common.Hash
	headHash   common.Hash
	head       uint64
	found      uint64
	foundHash  common.Hash
	valid      bool
}

func (c *commitmentLocation) locate(ctx context.Context, head *types.Block, commitment common.Hash, read func(uint64) (*types.Block, error)) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if head == nil {
		return 0, fmt.Errorf("missing canonical head")
	}
	lower := uint64(0)
	if c.valid && c.commitment == commitment {
		if c.found > 0 && c.found <= head.NumberU64() {
			block, err := read(c.found)
			if err != nil {
				return 0, err
			}
			if block != nil && block.Hash() == c.foundHash {
				return c.found, nil
			}
		} else if c.found == 0 && c.head <= head.NumberU64() {
			block, err := read(c.head)
			if err != nil {
				return 0, err
			}
			if block != nil && block.Hash() == c.headHash {
				if c.head == head.NumberU64() {
					return 0, nil
				}
				lower = c.head + 1
			}
		}
	}
	next := commitmentLocation{commitment: commitment, headHash: head.Hash(), head: head.NumberU64(), valid: true}
	for number := head.NumberU64(); ; number-- {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		block, err := read(number)
		if err != nil {
			return 0, err
		}
		if block == nil {
			return 0, fmt.Errorf("canonical block %d unavailable", number)
		}
		for _, value := range block.CMTS() {
			if value != nil && *value == commitment {
				next.found = number
				next.foundHash = block.Hash()
				*c = next
				return number, nil
			}
		}
		if number == lower {
			break
		}
	}
	*c = next
	return 0, nil
}
