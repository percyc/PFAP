package zktx

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestSMTCompatibilityVectors(t *testing.T) {
	s := NewSMTSnapshot()
	defer s.Close()
	// Captured from the original map-backed implementation before replacement.
	if s.Root().Hex() != "0xc1ca23e2a751b9225e67c4f14402f502db7a54633dfa58fe8a05120b72d49d1f" {
		t.Fatal("empty root changed")
	}
	vectors := map[int][2]string{
		1:  {"0xbf298876e296035d6b7d9cf92d43dc73b49c247ce9e806d18e0eef97dc545728", "0x6f31d7f8fcd39e0a55b23c5899cea8b1336efe3fad16d4cef011b0730bbe58fb"},
		2:  {"0x28ab28c84905854c3544d16958233b04da846003d01b50ed41930ce5c66e7c11", "0x9c0e7f093ad57e248e09c2c2fe4087e7d868bd628ea47796b0639f627b408b68"},
		16: {"0xab891fcb4679e6eec842c105249bfdda5c4ae93bf310a94edecd61845ac3ba14", "0x9aa1127db34c293a57b231e9de5a3f219589d8130f4173e64f0748dbc9ea2d9c"},
		32: {"0x2a414fedd9bfc8695980ca9cec6e3f7f6179b42e6fd5d836d71501a01500e52a", "0x020875ecb8fc93f7c309092bef9e04e5806b1060bc4fb541c47d0a63bbbe1bdf"},
	}
	for i := 1; i <= 32; i++ {
		c := common.BigToHash(big.NewInt(int64(i)))
		s.Insert(c)
		if want, ok := vectors[i]; ok {
			if s.Root().Hex() != want[0] || crypto.Keccak256Hash([]byte(s.Witness(c))).Hex() != want[1] {
				t.Fatalf("legacy root/witness mismatch at %d", i)
			}
		}
	}
}

func TestSMTSharedSnapshotBranches(t *testing.T) {
	base := NewSMTSnapshot()
	a, b := common.HexToHash("01"), common.HexToHash("02")
	base.Insert(a)
	original := base.Root()
	branch := base.Clone()
	base.Close() // A surviving clone must own its shared subtrees.
	defer branch.Close()
	branch.Insert(a)
	if branch.Root() != original {
		t.Fatal("duplicate insertion changed root")
	}
	other := branch.Clone()
	defer other.Close()
	branch.Insert(b)
	if other.Root() != original || other.Witness(b)[0] != '0' || branch.Witness(b)[0] != '1' {
		t.Fatal("branch insertion mutated a shared snapshot")
	}
	other.Insert(b)
	if branch.Root() != other.Root() {
		t.Fatal("equivalent branches disagree")
	}
}
