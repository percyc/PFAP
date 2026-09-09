package zktx

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

func privateRecordFixture(t *testing.T) PrivateAccountRecord {
	t.Helper()
	b, err := rlp.EncodeToBytes(SequenceS{Suquence1: testSequence(), Suquence2: testSequence(), Stage: Transfer})
	if err != nil {
		t.Fatal(err)
	}
	r := PrivateAccountRecord{Version: 1, Secret: common.HexToHash("1234"), State: b, Kind: "transaction", Signed: []byte{0xc0}}
	r.Transaction = crypto.Keccak256Hash(r.Signed)
	r.Checksum = accountChecksum(r)
	return r
}

func TestPrivateAccountAtomicRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account")
	r := privateRecordFixture(t)
	if err := writeAccountRecord(path, r); err != nil {
		t.Fatal(err)
	}
	got, state, err := readAccountRecord(path)
	if err != nil || !reflect.DeepEqual(*got, r) || state.Suquence2.Value != 7 {
		t.Fatalf("round trip failed: %v", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("secret file is not owner-only")
	}
	// Replacing a large record with a smaller payer reservation must not retain
	// stale bytes from the signed transaction (the old SN writer did).
	r.Kind, r.Signed, r.Transaction = "payer", nil, common.Hash{}
	if err := writeAccountRecord(path, r); err != nil {
		t.Fatal(err)
	}
	got, _, err = readAccountRecord(path)
	if err != nil || got.Kind != "payer" || len(got.Signed) != 0 {
		t.Fatalf("replacement failed: %v", err)
	}
	entries, _ := ioutil.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatal("temporary secret files leaked")
	}
}

func TestPrivateAccountRejectsTamperAndUnsafeTargets(t *testing.T) {
	r := privateRecordFixture(t)
	for _, mutate := range []func(*PrivateAccountRecord){
		func(x *PrivateAccountRecord) { x.Secret = common.HexToHash("5678") },
		func(x *PrivateAccountRecord) { x.Version = 99; x.Checksum = accountChecksum(*x) },
		func(x *PrivateAccountRecord) { x.Transaction = common.Hash{}; x.Checksum = accountChecksum(*x) },
		func(x *PrivateAccountRecord) { x.State = []byte{0xff}; x.Checksum = accountChecksum(*x) },
	} {
		bad := r
		mutate(&bad)
		if _, err := validAccountRecord(bad); err == nil {
			t.Fatal("corrupt account accepted")
		}
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := writeAccountRecord(target, r); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeAccountRecord(link, r); err == nil {
		t.Fatal("symlink write accepted")
	}
	if _, _, err := readAccountRecord(link); err == nil {
		t.Fatal("symlink read accepted")
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readAccountRecord(target); err == nil {
		t.Fatal("public secret file accepted")
	}
}

func TestPrivateAccountRestartAndFailureStayReserved(t *testing.T) {
	oldPath, oldFault, oldRecord, oldKey := accountPath, accountFault, accountRecord, AccountSK
	oldBefore, oldAfter, oldSNS, oldStage := SequenceNumber, SequenceNumberAfter, SNS, Stage
	defer func() {
		accountPath, accountFault, accountRecord, AccountSK = oldPath, oldFault, oldRecord, oldKey
		SequenceNumber, SequenceNumberAfter, SNS, Stage = oldBefore, oldAfter, oldSNS, oldStage
	}()
	dir := t.TempDir()
	if err := RestorePrivateAccount(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := PrivateAccountWritable(true); err != nil {
		t.Fatal(err)
	}
	key := common.HexToHash("1234")
	next := SequenceS{Suquence1: testSequence(), Suquence2: testSequence(), Stage: Transfer}
	if err := PersistPrivateTransition(next, &key, "payer", common.Hash{}, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate restart after persistence and before the RPC response/receiver tx.
	AccountSK = nil
	accountRecord = nil
	if err := RestorePrivateAccount(dir, false); err != nil {
		t.Fatal(err)
	}
	kind, hash, problem := PrivateAccountRecoveryInfo()
	if AccountSK == nil || *AccountSK != key || SequenceNumberAfter.Value != 7 || kind != "payer" || hash != (common.Hash{}) || problem != "" {
		t.Fatal("restart lost key, state or reservation")
	}
	if err := PrivateAccountWritable(true); err == nil {
		t.Fatal("repeat initialization permitted")
	}
	// Failed save must not alter authoritative state or permit subsequent writes.
	accountPath = filepath.Join(dir, "missing", "account")
	next.Suquence2.Value = 8
	if err := PersistPrivateTransition(next, &key, "payer", common.Hash{}, nil); err == nil {
		t.Fatal("failed save ignored")
	}
	if SequenceNumberAfter.Value != 7 || PrivateAccountWritable(false) == nil {
		t.Fatal("continued after persistence failure")
	}
	if err := RestorePrivateAccount(dir, false); err != nil || SequenceNumberAfter.Value != 7 {
		t.Fatal("previous durable checkpoint lost")
	}
	if err := RestorePrivateAccount(t.TempDir(), true); err == nil || PrivateAccountWritable(true) == nil || AccountSK != nil {
		t.Fatal("legacy missing secret silently replaced")
	}
}
