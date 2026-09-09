package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/pfap/lab/internal/model"
)

var errNodeUnavailable = errors.New("节点不可达或正在恢复，请等待节点恢复后再发送交易")

// Called inside the store update so recovery admission and transaction admission
// cannot race. Already submitted transactions are never replayed by recovery.
func transactionNodesAvailable(s model.State, tx model.Transaction) error {
	for _, exp := range s.Experiments {
		if exp.ID != tx.ExperimentID {
			continue
		}
		if exp.Status != "running" {
			return errors.New("experiment is not running")
		}
		var from, to *model.Node
		for i := range exp.Nodes {
			n := &exp.Nodes[i]
			if n.ID == tx.FromNode {
				from = n
			}
			if n.ID == tx.ToNode {
				to = n
			}
		}
		if from == nil {
			return errors.New("source node not found in experiment")
		}
		if from.Status != "running" {
			return errNodeUnavailable
		}
		if tx.WorkloadID != "" && (from.IsMiner || (from.Mining != nil && *from.Mining)) {
			return fmt.Errorf("%w: 矿工不参与自动交易", errNodeUnavailable)
		}
		if tx.Type != "public" && from.PrivateStateError != "" {
			return fmt.Errorf("%w: %s", errNodeUnavailable, from.PrivateStateError)
		}
		if tx.Type == "transfer" || tx.Type == "public" {
			if to == nil {
				return errors.New("destination node not found in experiment")
			}
			if tx.WorkloadID != "" && (to.IsMiner || (to.Mining != nil && *to.Mining)) {
				return fmt.Errorf("%w: 矿工不参与自动交易", errNodeUnavailable)
			}
			if tx.Type == "transfer" && from.ID == to.ID {
				return errors.New("payer and receiver must be different nodes")
			}
			// The receiver must execute a proof for Transfer. Public transfers
			// only need the receiver's persisted public address.
			if tx.Type == "transfer" && to.Status != "running" {
				return errNodeUnavailable
			}
			if tx.Type == "transfer" && to.PrivateStateError != "" {
				return fmt.Errorf("%w: %s", errNodeUnavailable, to.PrivateStateError)
			}
		}
		return nil
	}
	return errors.New("experiment not found")
}

func (a *API) beginNodeRecovery(experimentID, nodeID string) (model.Experiment, model.Node, map[string]model.Server, error) {
	var exp model.Experiment
	var node model.Node
	servers := map[string]model.Server{}
	// Recovery is a user operation too: queued maintenance must yield instead
	// of repeatedly winning the lock between every node's log rotation.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.waitForStopAdmission(ctx); err != nil {
		return exp, node, servers, err
	}
	defer a.lifecycleMu.Unlock()
	err := a.store.Update(func(s *model.State) error {
		for _, server := range s.Servers {
			servers[server.ID] = server
		}
		for i := range s.Experiments {
			e := &s.Experiments[i]
			if e.ID != experimentID {
				continue
			}
			if e.Status != "running" {
				return errors.New("只能恢复运行中实验的节点；已停止的实验请使用实验启动操作")
			}
			if e.MiningStatus == "updating" {
				return errors.New("矿工配置正在应用，请等待完成后再恢复节点")
			}
			for j := range e.Nodes {
				n := &e.Nodes[j]
				if n.ID != nodeID {
					continue
				}
				if n.Status == "recovering" {
					return errors.New("节点正在恢复，请勿重复操作")
				}
				if _, ok := servers[n.ServerID]; !ok {
					return errors.New("节点关联的服务器配置不存在")
				}
				n.Status = "recovering"
				n.Mining = nil
				n.RecoveryError = ""
				n.RecoveryStartedAt = time.Now()
				exp = *e
				exp.Nodes = append([]model.Node(nil), e.Nodes...)
				node = *n
				return nil
			}
			return os.ErrNotExist
		}
		return os.ErrNotExist
	})
	return exp, node, servers, err
}

func (a *API) recoverNode(w http.ResponseWriter, experimentID, nodeID string) {
	exp, node, servers, err := a.beginNodeRecovery(experimentID, nodeID)
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, os.ErrNotExist) {
			code = http.StatusNotFound
		}
		fail(w, code, err)
		return
	}
	a.emit(exp.ID, "info", "node-recovery", "开始恢复节点，保留原数据且不重发交易", map[string]any{"nodeId": node.ID})
	go a.runNodeRecovery(exp, node, servers)
	jsonOut(w, http.StatusAccepted, map[string]string{"status": "recovering", "nodeId": node.ID})
}

func (a *API) runNodeRecovery(exp model.Experiment, node model.Node, servers map[string]model.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	err := a.orch.RecoverNode(ctx, exp, node, servers, func(level, kind, message string, fields map[string]any) {
		if kind == "node-recovery-started" || kind == "node-recovery-existing" {
			// These legacy runtimes do not persist AccountSK. Do not claim
			// privacy continuity just because IPC and chain state are healthy.
			_ = a.store.Update(func(s *model.State) error {
				for i := range s.Experiments {
					if s.Experiments[i].ID != exp.ID {
						continue
					}
					for j := range s.Experiments[i].Nodes {
						n := &s.Experiments[i].Nodes[j]
						if n.ID == node.ID {
							previousSHA := n.RuntimeSHA
							if previousSHA == "" {
								previousSHA = s.Experiments[i].ArtifactSHA
							}
							versionChanged := false
							if sha, ok := fields["runtimeSha"].(string); ok && sha != "" {
								versionChanged = sha != previousSHA
								n.RuntimeSHA = sha
							}
							if (kind == "node-recovery-started" || versionChanged) && privateAccountInitialized(s.Transactions, exp.ID, *n) {
								n.RecoveryWarning = "节点进程已重启，原数据文件未删除。当前运行包尚未持久化独立的隐私账户秘密，重启后使用旧版回退逻辑；隐私交易连续性尚未验证，请勿重复 CreateAccount。"
							}
						}
					}
				}
				return nil
			})
		}
		a.emit(exp.ID, level, kind, message, fields)
	})
	if err == nil {
		err = a.sampleNode(ctx, exp, node, servers[node.ServerID], "recovery")
	}
	a.completeNodeRecovery(exp.ID, node.ID, err)
}

func (a *API) completeNodeRecovery(experimentID, nodeID string, recoveryErr error) {
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			if s.Experiments[i].ID != experimentID {
				continue
			}
			for j := range s.Experiments[i].Nodes {
				n := &s.Experiments[i].Nodes[j]
				if n.ID != nodeID {
					continue
				}
				if recoveryErr != nil {
					n.Status = "unreachable"
					n.RecoveryError = recoveryErr.Error()
					n.StateError = "恢复未完成，请查看恢复详情。"
				} else {
					n.Status = "running"
					n.RecoveryError = ""
				}
			}
		}
		return nil
	})
	if recoveryErr != nil {
		a.emit(experimentID, "error", "node-recovery", fmt.Sprintf("节点恢复未完成：%v", recoveryErr), map[string]any{"nodeId": nodeID})
	} else {
		a.emit(experimentID, "info", "node-recovery", "节点已恢复连接；原实验数据保留，未重发任何交易", map[string]any{"nodeId": nodeID})
	}
}
