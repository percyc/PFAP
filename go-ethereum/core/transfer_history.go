package core

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/zktx"
)

// Transfer anchors reuse the formerly unused CMTBlock field: exactly one
// block height. The root must match that ancestor of the validating parent.
// This is a consensus rule change: use a fresh chain with uniformly upgraded nodes.
type CommitmentBlockReader func(common.Hash, uint64) *types.Block

type commitmentSnapshot struct {
	hash common.Hash
	tree *zktx.SMTSnapshot
}

var transferHistory = struct {
	sync.Mutex
	snapshots []commitmentSnapshot
	roots     map[common.Hash]common.Hash
}{roots: make(map[common.Hash]common.Hash)}

// historyTree is called under transferHistory's lock. Snapshots are bounded and
// reconstructed exclusively from immutable block bodies, never the global SMT.
func historyTree(block *types.Block, read CommitmentBlockReader) (*zktx.SMTSnapshot, error) {
	if block == nil {
		return nil, errors.New("historical commitment block unavailable")
	}
	target := block.Hash()
	var pending []*types.Block
	var base *zktx.SMTSnapshot
	for {
		for _, entry := range transferHistory.snapshots {
			if entry.hash == block.Hash() {
				base = entry.tree
				break
			}
		}
		if base != nil {
			break
		}
		pending = append(pending, block)
		if block.NumberU64() == 0 {
			break
		}
		parent := read(block.ParentHash(), block.NumberU64()-1)
		if parent == nil || parent.Hash() != block.ParentHash() || parent.NumberU64()+1 != block.NumberU64() {
			return nil, errors.New("incomplete commitment history")
		}
		block = parent
	}
	if len(pending) == 0 {
		return base, nil
	}
	var commitments []common.Hash
	seen := make(map[common.Hash]bool)
	for i := len(pending) - 1; i >= 0; i-- {
		for _, tx := range pending[i].Transactions() {
			for _, c := range transactionCommitments(tx) {
				if !seen[*c] {
					commitments = append(commitments, *c)
					seen[*c] = true
				}
			}
		}
	}
	// Empty blocks do not change the tree and do not need a native map copy.
	if base != nil && len(commitments) == 0 {
		return base, nil
	}
	var tree *zktx.SMTSnapshot
	if base == nil {
		tree = zktx.NewSMTSnapshot()
	} else {
		tree = base.Clone()
	}
	for _, c := range commitments {
		tree.Insert(c)
	}
	transferHistory.snapshots = append(transferHistory.snapshots, commitmentSnapshot{target, tree})
	if len(transferHistory.snapshots) > 4 {
		transferHistory.snapshots[0].tree.Close()
		transferHistory.snapshots = transferHistory.snapshots[1:]
	}
	return tree, nil
}

// CommitmentWitnessAt returns a copied witness; callers may release the cache
// lock before the expensive proving operation without races with new blocks.
func CommitmentWitnessAt(block *types.Block, read CommitmentBlockReader, commitment common.Hash) (common.Hash, string, error) {
	transferHistory.Lock()
	defer transferHistory.Unlock()
	tree, err := historyTree(block, read)
	if err != nil {
		return common.Hash{}, "", err
	}
	root, witness := tree.Root(), tree.Witness(commitment)
	if len(witness) != 16705 || witness[0] != '1' {
		return common.Hash{}, "", errors.New("account commitment not present at Transfer anchor; wait for account confirmation before creating payer proof")
	}
	rememberCommitmentRoot(block.Hash(), root)
	return root, witness, nil
}

func rememberCommitmentRoot(hash, root common.Hash) {
	if len(transferHistory.roots) >= 4096 {
		transferHistory.roots = make(map[common.Hash]common.Hash)
	}
	transferHistory.roots[hash] = root
}

func transferAnchor(head *types.Block, read CommitmentBlockReader, height uint64) (*types.Block, error) {
	if head == nil || height > head.NumberU64() {
		return nil, errors.New("Transfer anchor is ahead of locally verified chain")
	}
	for head.NumberU64() > height {
		parent := read(head.ParentHash(), head.NumberU64()-1)
		if parent == nil || parent.Hash() != head.ParentHash() || parent.NumberU64()+1 != head.NumberU64() {
			return nil, errors.New("Transfer anchor ancestry unavailable")
		}
		head = parent
	}
	return head, nil
}

func ValidateTransferRoot(head *types.Block, read CommitmentBlockReader, heights []uint64, root common.Hash) (*types.Block, error) {
	if len(heights) != 1 {
		return nil, errors.New("Transfer requires exactly one historical anchor height; legacy unanchored Transfer is unsupported")
	}
	anchor, err := transferAnchor(head, read, heights[0])
	if err != nil {
		return nil, err
	}
	transferHistory.Lock()
	defer transferHistory.Unlock()
	expected, ok := transferHistory.roots[anchor.Hash()]
	if !ok {
		tree, err := historyTree(anchor, read)
		if err != nil {
			return nil, err
		}
		expected = tree.Root()
		rememberCommitmentRoot(anchor.Hash(), expected)
	}
	if root != expected {
		return nil, fmt.Errorf("Transfer root is not the verified historical root at block %d", heights[0])
	}
	return anchor, nil
}

func ValidateTransferShape(tx *types.Transaction) error {
	if tx.ZKSN() == nil || tx.ZKSNS() == nil || tx.ZKCMT() == nil || tx.ZKCMT2() == nil || tx.ZKCMTS() == nil || len(tx.ZKProof()) != zktx.TransferProofHexSize || len(tx.ZKProof2()) != zktx.TransferProofHexSize {
		return errors.New("Transfer requires both complete payer and receiver proofs")
	}
	if *tx.ZKSN() == *tx.ZKSNS() {
		return errors.New("Transfer payer and receiver must have distinct serial numbers")
	}
	return nil
}

func validateTransferSerials(tx *types.Transaction, exists func(common.Address) bool) error {
	for _, sn := range []*common.Hash{tx.ZKSN(), tx.ZKSNS()} {
		if sn == nil || exists(common.BytesToAddress(sn.Bytes())) {
			return errors.New("Transfer serial number missing or already spent")
		}
	}
	return nil
}
