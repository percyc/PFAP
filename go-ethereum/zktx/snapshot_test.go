package zktx

import (
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestSMTSnapshotIsolation(t *testing.T) {
	a, b := common.HexToHash("01"), common.HexToHash("02")
	s := NewSMTSnapshot()
	defer s.Close()
	s.Insert(a)
	root, witness := s.Root(), s.Witness(a)
	clone := s.Clone()
	defer clone.Close()
	clone.Insert(b)
	InsertCMT(&b)
	if s.Root() != root || s.Witness(a) != witness || clone.Root() == root {
		t.Fatal("snapshot mutated")
	}
	if s.Witness(b)[0] != '0' || clone.Witness(b)[0] != '1' {
		t.Fatal("membership isolation failed")
	}
}

// Opt-in because this uses the real unchanged proving key and takes minutes.
func TestTransferHistoricalWitnessRealProof(t *testing.T) {
	if os.Getenv("PFAP_TEST_REAL_PROOFS") != "1" {
		t.Skip("set PFAP_TEST_REAL_PROOFS=1 with PFAP_PRFKEY_DIR")
	}
	skA, skB, rA, rB, rs := NewRandomHash(), NewRandomHash(), NewRandomHash(), NewRandomHash(), NewRandomHash()
	snA, snB := ComputePRF(skA.Bytes(), common.Hash{}.Bytes()), ComputePRF(skB.Bytes(), common.Hash{}.Bytes())
	cmtA, cmtB := GenCMT(10, snA.Bytes(), rA.Bytes()), GenCMT(10, snB.Bytes(), rB.Bytes())
	snapshot := NewSMTSnapshot()
	defer snapshot.Close()
	snapshot.Insert(*cmtA)
	snapshot.Insert(*cmtB)
	root, witnessA, witnessB := snapshot.Root(), snapshot.Witness(*cmtA), snapshot.Witness(*cmtB)
	newSNA, newSNB := ComputePRF(skA.Bytes(), snA.Bytes()), ComputePRF(skB.Bytes(), snB.Bytes())
	newRA, newRB := NewRandomHash(), NewRandomHash()
	newCMTA, newCMTB := GenCMT(9, newSNA.Bytes(), newRA.Bytes()), GenCMT(11, newSNB.Bytes(), newRB.Bytes())
	started := time.Now()
	proofA := GenTransferProofAt(10, rA, newSNA, newRA, cmtA, snA, newCMTA, 9, skA, 1, rs, root.Bytes(), 0, witnessA)
	t.Log("payer proof duration", time.Since(started))
	// Another commitment changes both the current global tree and a later chain snapshot.
	other := NewRandomHash()
	InsertCMT(other)
	later := snapshot.Clone()
	defer later.Close()
	later.Insert(*other)
	latestRoot := later.Root()
	if latestRoot == root {
		t.Fatal("test root did not advance")
	}
	started = time.Now()
	proofB := GenTransferProofAt(10, rB, newSNB, newRB, cmtB, snB, newCMTB, 11, skB, 1, rs, root.Bytes(), 1, witnessB)
	t.Log("receiver proof duration", time.Since(started))
	cmtS := GenCMTStransfer(1, rs)
	started = time.Now()
	if err := VerifyTransferProof(cmtS, snA, newCMTA, &root, 1, 0, proofA); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTransferProof(cmtS, snB, newCMTB, &root, 1, 1, proofB); err != nil {
		t.Fatal(err)
	}
	t.Log("both proof verifications", time.Since(started))
	if VerifyTransferProof(cmtS, snA, newCMTA, &latestRoot, 1, 0, proofA) == nil {
		t.Fatal("proof accepted against a different root")
	}
	// A mismatched historical witness must fail before producing a usable proof.
	bad := GenTransferProofAt(10, rA, newSNA, newRA, cmtA, snA, newCMTA, 9, skA, 1, rs, latestRoot.Bytes(), 0, witnessA)
	if VerifyTransferProof(cmtS, snA, newCMTA, &latestRoot, 1, 0, bad) == nil {
		t.Fatal("mismatched witness accepted")
	}
}

func BenchmarkSMTSnapshotClone256(b *testing.B) {
	s := NewSMTSnapshot()
	defer s.Close()
	for i := 0; i < 256; i++ {
		s.Insert(common.BigToHash(big.NewInt(int64(i + 1))))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clone := s.Clone()
		clone.Close()
	}
}
