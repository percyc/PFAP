package deps

import (
	"strings"
	"testing"
)

func TestWeb3HasRevertTransferState(t *testing.T) {
	data, err := Asset("web3.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "revertTransferState") {
		t.Fatal("web3.js asset does NOT contain revertTransferState")
	}
	t.Logf("web3.js asset size: %d, contains revertTransferState: TRUE", len(s))
}
