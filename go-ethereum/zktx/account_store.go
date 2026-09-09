package zktx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// One private account per geth process. Callers hold AccountMu across admission,
// proof generation and durable transition; unrelated nodes remain concurrent.
var AccountMu sync.Mutex
var accountPath string
var accountFault error
var accountRecord *PrivateAccountRecord

// PrivateAccountRecord is secret material, never an RPC response or log field.
// The signed transaction is saved BEFORE submission, including the next state.
// A failed/ambiguous submission deliberately leaves this record reserved.
type PrivateAccountRecord struct {
	Version     int         `json:"version"`
	Secret      common.Hash `json:"secret"`
	State       []byte      `json:"state"`
	Kind        string      `json:"kind"`
	Transaction common.Hash `json:"transaction"`
	Signed      []byte      `json:"signed,omitempty"`
	Checksum    string      `json:"checksum"`
}

func accountChecksum(r PrivateAccountRecord) string {
	r.Checksum = ""
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func validAccountRecord(r PrivateAccountRecord) (*SequenceS, error) {
	if r.Version != 1 || r.Secret == (common.Hash{}) || r.Checksum != accountChecksum(r) {
		return nil, errors.New("invalid private account record version, key or checksum")
	}
	if r.Kind != "payer" && r.Kind != "transaction" {
		return nil, errors.New("invalid private account transition")
	}
	if r.Kind == "transaction" && (len(r.Signed) == 0 || crypto.Keccak256Hash(r.Signed) != r.Transaction) {
		return nil, errors.New("private account signed transaction mismatch")
	}
	if r.Kind == "payer" && (len(r.Signed) != 0 || r.Transaction != (common.Hash{})) {
		return nil, errors.New("invalid payer reservation")
	}
	var state SequenceS
	if err := rlp.DecodeBytes(r.State, &state); err != nil {
		return nil, errors.New("invalid private account sequence encoding")
	}
	for _, seq := range []*Sequence{&state.Suquence1, &state.Suquence2} {
		if seq.SN == nil || seq.CMT == nil || seq.Random == nil {
			return nil, errors.New("incomplete private account sequence")
		}
	}
	return &state, nil
}

func readAccountRecord(path string) (*PrivateAccountRecord, *SequenceS, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, nil, errors.New("private account record must be a regular owner-only file")
	}
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var record PrivateAccountRecord
	if err := json.Unmarshal(b, &record); err != nil {
		return nil, nil, errors.New("invalid private account record JSON")
	}
	state, err := validAccountRecord(record)
	return &record, state, err
}

// Atomic replacement plus file AND parent-directory fsync. No authoritative
// state is overwritten in-place. Failure is surfaced and disables further writes.
func writeAccountRecord(path string, record PrivateAccountRecord) error {
	record.Checksum = accountChecksum(record)
	if _, err := validAccountRecord(record); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return errors.New("unsafe private account record target")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := ioutil.TempFile(filepath.Dir(path), ".private-account-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// RestorePrivateAccount runs at startup after legacy SN is inspected. A legacy
// initialized account without its random secret remains read-only, never migrated
// using the old public-address fallback. SN is preserved for forensic inspection.
func RestorePrivateAccount(dir string, legacyInitialized bool) error {
	accountPath = filepath.Join(dir, "private-account-v1.json")
	accountRecord, accountFault, AccountSK = nil, nil, nil
	r, state, err := readAccountRecord(accountPath)
	if os.IsNotExist(err) && !legacyInitialized {
		return nil
	}
	if os.IsNotExist(err) {
		err = errors.New("legacy private account secret is missing; preserve data and do not initialize or replay")
	}
	if err != nil {
		accountFault = err
		return err
	}
	accountRecord = r
	AccountSK = &r.Secret
	SequenceNumber, SequenceNumberAfter, SNS, Stage = &state.Suquence1, &state.Suquence2, state.SNS, state.Stage
	return nil
}

func PrivateAccountWritable(create bool) error {
	if accountFault != nil {
		return fmt.Errorf("private account is read-only: %v", accountFault)
	}
	if accountPath == "" {
		return errors.New("private account store is not open")
	}
	if create {
		if accountRecord != nil || AccountSK != nil {
			return errors.New("private account already initialized; do not repeat CreateAccount")
		}
	} else if accountRecord == nil || AccountSK == nil {
		return errors.New("private account has no durable secret; CreateAccount is required only for a fresh account")
	}
	return nil
}

// PersistPrivateTransition must succeed before broadcasting a signed transaction
// or returning a payer proof. The authoritative record itself is the write-ahead
// journal; no second post-broadcast write is required to recover the next state.
func PersistPrivateTransition(next SequenceS, key *common.Hash, kind string, hash common.Hash, signed []byte) error {
	if accountFault != nil || accountPath == "" || key == nil {
		return errors.New("private account persistence unavailable")
	}
	b, err := rlp.EncodeToBytes(next)
	if err != nil {
		return err
	}
	r := PrivateAccountRecord{Version: 1, Secret: *key, State: b, Kind: kind, Transaction: hash, Signed: signed}
	r.Checksum = accountChecksum(r)
	if err = writeAccountRecord(accountPath, r); err != nil {
		accountFault = err // Rename may have succeeded: never continue from old memory.
		return fmt.Errorf("private account persistence failed; no broadcast permitted: %v", err)
	}
	state, _ := validAccountRecord(r)
	accountRecord, AccountSK = &r, &r.Secret
	SequenceNumber, SequenceNumberAfter, SNS, Stage = &state.Suquence1, &state.Suquence2, state.SNS, state.Stage
	return nil
}

// Metadata only: never expose Secret, State, or Signed via the web/RPC.
func PrivateAccountRecoveryInfo() (string, common.Hash, string) {
	if accountFault != nil {
		return "read-only", common.Hash{}, accountFault.Error()
	}
	if accountRecord == nil {
		return "uninitialized", common.Hash{}, ""
	}
	return accountRecord.Kind, accountRecord.Transaction, ""
}
