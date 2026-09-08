package orchestrator

import (
	"context"
	"github.com/pfap/lab/internal/model"
	"regexp"
	"strconv"
	"strings"
)

var blockValidationPattern = regexp.MustCompile(`PFAP_BLOCK_EXECUTION_VALIDATION hash=(0x[0-9a-fA-F]{64}) us=([0-9]+)`)

func ParseBlockValidationTimings(text string) map[string]int64 {
	result := map[string]int64{}
	for _, m := range blockValidationPattern.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.ParseInt(m[2], 10, 64); err == nil {
			result[strings.ToLower(m[1])] = n
		}
	}
	return result
}
func (o Orchestrator) BlockValidationTimings(ctx context.Context, e model.Experiment, n model.Node, s model.Server) (map[string]int64, error) {
	logPath := strings.TrimRight(s.WorkDir, "/") + "/experiments/" + e.ID + "/" + s.ID + "/node" + strconv.Itoa(n.LocalIndex) + "/geth.log"
	out, err := o.Remote.Run(ctx, s, retainedLogTail(logPath, 4000))
	if err != nil {
		return nil, err
	}
	return ParseBlockValidationTimings(out), nil
}
