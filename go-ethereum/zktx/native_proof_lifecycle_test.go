package zktx

import (
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// Exercises native return-buffer ownership with real keys for the three other
// circuits. All state is synthetic and local to the test process.
func TestNativeProofLifecycle(t *testing.T) {
	if os.Getenv("PFAP_TEST_REAL_PROOFS") != "1" {
		t.Skip("opt-in real proofs")
	}
	sk, r0 := NewRandomHash(), NewRandomHash()
	sn0 := ComputePRF(sk.Bytes(), common.Hash{}.Bytes())
	c0 := GenCMT(0, sn0.Bytes(), r0.Bytes())
	if err := VerifyCreateAccountProof(c0, GenCreateAccountProof(sk, r0, sn0, c0)); err != nil {
		t.Fatal(err)
	}
	InsertCMT(c0)
	root0 := GenRT(nil)
	sn1, r1 := ComputePRF(sk.Bytes(), sn0.Bytes()), NewRandomHash()
	c1 := GenCMT(100, sn1.Bytes(), r1.Bytes())
	mint := GenMintProof(0, r0, sn1, r1, c0, sn0, c1, 100, sk, nil, root0.Bytes())
	if err := VerifyMintProof(sn0, &root0, c1, 100, mint); err != nil {
		t.Fatal(err)
	}
	InsertCMT(c1)
	root1 := GenRT(nil)
	sn2, r2 := ComputePRF(sk.Bytes(), sn1.Bytes()), NewRandomHash()
	c2 := GenCMT(99, sn2.Bytes(), r2.Bytes())
	redeem := GenRedeemProof(100, r1, sn2, r2, c1, sn1, c2, 99, sk, nil, root1.Bytes())
	if err := VerifyRedeemProof(sn1, &root1, c2, 1, redeem); err != nil {
		t.Fatal(err)
	}
}
