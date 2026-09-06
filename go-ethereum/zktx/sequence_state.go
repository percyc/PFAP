package zktx

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/ethereum/go-ethereum/rlp"
)

// ReadSequenceState reads the first record from the legacy SN file without
// modifying it. Legacy writers overwrite the first line without truncating the
// file, so bytes after a complete first record are deliberately ignored.
// A nil result denotes a genuinely empty, not-yet-initialized state file.
func ReadSequenceState(reader io.Reader) (*SequenceS, error) {
	buffer := bufio.NewReader(reader)
	line, err := buffer.ReadString('\n')
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read SN state: %v", err)
	}
	encoded := strings.TrimSpace(line)
	if encoded == "" {
		// A blank first line must not hide an otherwise nonempty corrupt file.
		remaining, err := io.ReadAll(buffer)
		if err != nil {
			return nil, fmt.Errorf("read SN state: %v", err)
		}
		if strings.TrimSpace(string(remaining)) != "" {
			return nil, fmt.Errorf("invalid SN state: empty first record in nonempty file")
		}
		return nil, nil
	}
	data, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode SN hex: %v", err)
	}
	var state SequenceS
	if err := rlp.DecodeBytes(data, &state); err != nil {
		return nil, fmt.Errorf("decode SN state: %v", err)
	}
	return &state, nil
}
