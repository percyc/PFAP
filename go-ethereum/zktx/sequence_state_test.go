package zktx

import (
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

func testSequence() Sequence {
	sn := common.HexToHash("01")
	cmt := common.HexToHash("02")
	random := common.HexToHash("03")
	return Sequence{SN: &sn, CMT: &cmt, Random: &random, Value: 7, Valid: true}
}

func TestSequenceStateRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sns   *Sequence
		stage uint8
	}{
		{name: "Origin with nil SNS", stage: Origin},
		{name: "Transfer with SNS", sns: func() *Sequence { s := testSequence(); return &s }(), stage: Transfer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := SequenceS{testSequence(), testSequence(), tc.sns, big.NewInt(11), big.NewInt(12), tc.stage}
			data, err := rlp.EncodeToBytes(original)
			if err != nil {
				t.Fatal(err)
			}
			var decoded SequenceS
			if err := rlp.DecodeBytes(data, &decoded); err != nil {
				t.Fatalf("decode failed: %v", err)
			}
			if !reflect.DeepEqual(original, decoded) {
				t.Fatal("round trip changed private account state")
			}
		})
	}
}

func TestReadSequenceStateLegacyEncoding(t *testing.T) {
	// The pre-fix struct writes a nil SNS as an empty list. The decoder fix must
	// read those existing bytes without changing the on-disk representation.
	type legacySequenceState struct {
		Before Sequence
		After  Sequence
		SNS    *Sequence
		PKBX   *big.Int
		PKBY   *big.Int
		Stage  uint8
	}
	legacy := legacySequenceState{Before: testSequence(), After: testSequence(), Stage: Origin}
	data, err := rlp.EncodeToBytes(legacy)
	if err != nil {
		t.Fatal(err)
	}
	encoded := hex.EncodeToString(data)
	for _, suffix := range []string{"", "\n", "\r\n", "\nleftover bytes from legacy non-truncating writer"} {
		state, err := ReadSequenceState(strings.NewReader(" \t" + encoded + " \t" + suffix))
		if err != nil || state == nil {
			t.Fatalf("legacy record failed: %v", err)
		}
		if state.SNS != nil || state.Stage != Origin || !reflect.DeepEqual(state.Suquence1, legacy.Before) || !reflect.DeepEqual(state.Suquence2, legacy.After) {
			t.Fatal("legacy record decoded to different private account state")
		}
		reencoded, err := rlp.EncodeToBytes(state)
		if err != nil || !reflect.DeepEqual(data, reencoded) {
			t.Fatalf("decoder fix changed legacy encoding: %v", err)
		}
	}
}

func TestReadSequenceStateEmptyAndMalformed(t *testing.T) {
	for _, input := range []string{"", " \t", "\n", "\r\n \t"} {
		state, err := ReadSequenceState(strings.NewReader(input))
		if state != nil || err != nil {
			t.Fatalf("empty state rejected: %v", err)
		}
	}
	for _, input := range []string{"not hex\n", "f\n", "c0\n", "\nnonempty corrupted state", "80", "00"} {
		state, err := ReadSequenceState(strings.NewReader(input))
		if err == nil || state != nil {
			t.Fatal("malformed state was accepted or reset to an initial account")
		}
	}
	if state, err := ReadSequenceState(sequenceErrorReader{}); err == nil || state != nil {
		t.Fatal("state read error was ignored")
	}
}

type sequenceErrorReader struct{}

func (sequenceErrorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

var _ io.Reader = sequenceErrorReader{}
