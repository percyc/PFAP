package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"

	"github.com/pfap/lab/internal/model"
)

var proofJournalID = regexp.MustCompile(`^tx-[a-f0-9]+$`)

// Retain the returned public proof before any receiver-side work. This is
// evidence for explicit continuation, NOT permission to replay an unknown RPC.
// Never store private keys or witnesses, and never overwrite an earlier proof.
func (a *API) saveTransferProof(tx model.Transaction, runtime, raw string) error {
	if !proofJournalID.MatchString(tx.ID) || tx.Type != "transfer" || !json.Valid([]byte(raw)) {
		return errors.New("invalid transfer proof journal")
	}
	data, err := json.Marshal(struct {
		TransactionID string          `json:"transactionId"`
		ExperimentID  string          `json:"experimentId"`
		FromNode      string          `json:"fromNode"`
		ToNode        string          `json:"toNode"`
		Value         string          `json:"value"`
		Runtime       string          `json:"runtimeSha"`
		Proof         json.RawMessage `json:"proof"`
	}{tx.ID, tx.ExperimentID, tx.FromNode, tx.ToNode, tx.Value, runtime, json.RawMessage(raw)})
	if err != nil {
		return err
	}
	dir := filepath.Join(a.store.DataDir(), "transfer-proofs")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(tmp, filepath.Join(dir, tx.ID+".json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
