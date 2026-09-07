package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pfap/lab/internal/model"
)

// SetMining changes only this process's mining role. The runtime starts its
// miner asynchronously, so a successful console call must be followed by an
// observed eth.mining value, not treated as an acknowledgement by itself.
func (o Orchestrator) SetMining(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, enabled bool) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	expression := "miner.stop(); eth.mining"
	if enabled {
		out, err := o.Attach(ctx, exp, node, server, "eth.mining")
		if err != nil {
			return fmt.Errorf("read mining state on %s: %w (%s)", node.Name, err, strings.TrimSpace(out))
		}
		mining, ok := consoleBoolean(out)
		if !ok {
			return fmt.Errorf("set mining=true on %s: console did not return eth.mining: %s", node.Name, strings.TrimSpace(out))
		}
		if mining {
			return nil
		}
		if err := o.CheckDisk(ctx, server, minerServerDiskRequiredBytes(exp, node, server)); err != nil {
			return fmt.Errorf("start mining on %s: %w", node.Name, err)
		}
		expression = "miner.setEtherbase(eth.accounts[0]); miner.start(1); eth.mining"
	}
	for {
		out, err := o.Attach(ctx, exp, node, server, expression)
		if err != nil {
			return fmt.Errorf("set mining=%t on %s: %w (%s)", enabled, node.Name, err, strings.TrimSpace(out))
		}
		mining, ok := consoleBoolean(out)
		if !ok {
			return fmt.Errorf("set mining=%t on %s: console did not return eth.mining: %s", enabled, node.Name, strings.TrimSpace(out))
		}
		if mining == enabled {
			return nil
		}
		expression = "eth.mining"
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("set mining=%t on %s: state was not confirmed: %w", enabled, node.Name, ctx.Err())
		case <-timer.C:
		}
	}
}

func consoleBoolean(out string) (bool, bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	switch strings.TrimSpace(lines[len(lines)-1]) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}
