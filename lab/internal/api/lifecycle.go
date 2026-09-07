package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/orchestrator"
)

var errLifecycleBusy = errors.New("正在执行实验操作或磁盘维护，请稍后重试")

func plannedNodes(exp model.Experiment) ([]model.Node, error) {
	return model.ResolvePlannedMiners(exp, nil)
}

func (a *API) beginLifecycle(experimentID, action string) (model.Experiment, map[string]model.Server, string, error) {
	if action == "stop" {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := a.waitForStopAdmission(ctx); err != nil {
			return model.Experiment{}, nil, "", err
		}
	} else if !a.lifecycleMu.TryLock() {
		return model.Experiment{}, nil, "", errLifecycleBusy
	}
	defer a.lifecycleMu.Unlock()
	var exp model.Experiment
	servers := map[string]model.Server{}
	a.store.View(func(s model.State) {
		for _, server := range s.Servers {
			servers[server.ID] = server
		}
		for _, e := range s.Experiments {
			if e.ID == experimentID {
				exp = e
			}
		}
	})
	if exp.ID == "" {
		return exp, servers, "", os.ErrNotExist
	}
	if action != "deploy" && action != "start" && action != "resume" && action != "stop" {
		return exp, servers, "", errors.New("unknown lifecycle action")
	}
	if exp.MiningStatus == "updating" {
		return exp, servers, "", errors.New("矿工配置正在应用，请等待完成")
	}
	for _, n := range exp.Nodes {
		if n.Status == "recovering" {
			return exp, servers, "", errors.New("节点正在恢复，请等待完成")
		}
	}
	if exp.Status == "deploying" || exp.Status == "resuming" || exp.Status == "stopping" {
		return exp, servers, "", errors.New("实验操作正在执行，请勿重复提交")
	}
	operation := action
	if action == "stop" {
		if exp.Status != "running" && exp.Status != "failed" && exp.Status != "interrupted" && exp.Status != "stop-failed" {
			return exp, servers, "", errors.New("实验不在可停止状态")
		}
		operation = "stopping"
	} else {
		if exp.Status == "running" {
			return exp, servers, "", errors.New("实验已运行，请使用单节点恢复")
		}
		if exp.Status != "draft" && exp.Status != "failed" && exp.Status != "stopped" && exp.Status != "interrupted" && exp.Status != "stop-failed" {
			return exp, servers, "", errors.New("实验当前状态不允许部署或继续运行")
		}
		fresh := (exp.Status == "draft" || exp.Status == "failed") && exp.ArtifactSHA == "" && len(exp.Nodes) == 0 && exp.StartedAt.IsZero()
		if action == "resume" || !fresh {
			if len(exp.Nodes) == 0 || !transactionHashPattern.MatchString("0x"+exp.ArtifactSHA) {
				return exp, servers, "", errors.New("缺少原节点清单或原运行包 SHA，不能把重新部署当作恢复；请先核验停止结果，必要时新建实验")
			}
			operation = "resuming"
		} else {
			file, err := os.Open(exp.ArtifactPath)
			if err != nil {
				return exp, servers, "", fmt.Errorf("read deployment runtime: %w", err)
			}
			hash := sha256.New()
			_, readErr := io.Copy(hash, file)
			closeErr := file.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return exp, servers, "", err
			}
			exp.ArtifactSHA = hex.EncodeToString(hash.Sum(nil))
			nodes, err := model.ResolvePlannedMiners(exp, servers)
			if err != nil {
				return exp, servers, "", err
			}
			exp.Nodes, operation = nodes, "deploying"
		}
	}
	err := a.store.Update(func(s *model.State) error {
		servers = make(map[string]model.Server, len(s.Servers))
		for _, srv := range s.Servers {
			servers[srv.ID] = srv
		}
		for i := range s.Experiments {
			e := &s.Experiments[i]
			if e.ID != experimentID {
				continue
			}
			if operation == "deploying" {
				for _, p := range e.Placements {
					if _, ok := servers[p.ServerID]; !ok {
						return errors.New("部署服务器配置不存在")
					}
				}
				// Pin desired roles against the same server metadata snapshot
				// returned to deployment and its disk preflight.
				nodes, err := model.ResolvePlannedMiners(exp, servers)
				if err != nil {
					return err
				}
				exp.Nodes = nodes
				e.ArtifactSHA, e.Nodes = exp.ArtifactSHA, exp.Nodes
			}
			e.Status, e.Error = operation, ""
			if operation == "resuming" {
				for j := range e.Nodes {
					e.Nodes[j].Status, e.Nodes[j].Mining = "unknown", nil
					e.Nodes[j].RecoveryStartedAt = time.Now()
				}
			}
			if operation == "stopping" {
				for j := range s.Workloads {
					w := &s.Workloads[j]
					if w.ExperimentID == e.ID && (w.Status == "queued" || w.Status == "running" || w.Status == "draining") {
						w.StopRequested, w.Status, w.FinishedAt = true, "interrupted", time.Now()
						if w.SubmissionStoppedAt.IsZero() {
							w.SubmissionStoppedAt = w.FinishedAt
						}
						w.Error = "实验正在停止，已终止新增投递；未确认交易需单独核验。"
					}
				}
				for j := range s.Transactions {
					tx := &s.Transactions[j]
					if tx.ExperimentID != e.ID {
						continue
					}
					if tx.Status == "queued" {
						tx.Status, tx.Error = "cancelled", "实验停止，未开始的交易已取消。"
					} else if tx.Status == "proving" || tx.Status == "submitted" {
						tx.Status, tx.Error, tx.ConfirmedAt = "unknown", "实验停止期间执行结果待核验；不重发、不盲目回滚。", time.Time{}
					}
				}
			}
			exp = *e
			exp.Nodes = append([]model.Node(nil), e.Nodes...)
			return nil
		}
		return os.ErrNotExist
	})
	return exp, servers, operation, err
}

// A user stop waits for the current bounded maintenance lease instead of
// racing each node's periodic log check. Never release another owner's lock.
func (a *API) waitForStopAdmission(ctx context.Context) error {
	a.stopWaiters.Add(1)
	defer a.stopWaiters.Add(-1)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return errors.New("等待当前实验操作或磁盘维护结束超时，尚未提交停止；请检查维护任务后重试")
		}
		if a.lifecycleMu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (a *API) lifecycleAction(w http.ResponseWriter, experimentID, action string) {
	if action != "deploy" && action != "start" && action != "resume" && action != "stop" {
		fail(w, 404, errors.New("unknown action"))
		return
	}
	exp, servers, operation, err := a.beginLifecycle(experimentID, action)
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, os.ErrNotExist) {
			code = http.StatusNotFound
		}
		if a.store.Health().Degraded {
			code = http.StatusServiceUnavailable
		}
		fail(w, code, err)
		return
	}
	switch operation {
	case "deploying":
		go a.deploy(experimentID, exp, servers)
	case "resuming":
		go a.resumeExperiment(exp, servers)
	case "stopping":
		go a.stop(experimentID, exp, servers)
	}
	jsonOut(w, http.StatusAccepted, map[string]string{"status": operation})
}

func (a *API) resumeExperiment(exp model.Experiment, servers map[string]model.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	nodes, resumeErr := a.orch.Resume(ctx, &exp, servers, func(level, kind, message string, fields map[string]any) {
		a.emit(exp.ID, level, kind, message, fields)
	})
	if len(nodes) == 0 {
		_ = a.setExperiment(exp.ID, "interrupted", fmt.Sprintf("原实验未恢复：%v", resumeErr), nil, "")
		return
	}
	a.store.View(func(s model.State) {
		for i := range nodes {
			// Do not expose cached account state as ready before the first live
			// sample of the resumed process has checked its privacy state.
			nodes[i].PrivateStateError = "原目录已恢复运行，正在核验隐私账户状态，请等待首次状态检查。"
			if privateAccountInitialized(s.Transactions, exp.ID, nodes[i]) {
				nodes[i].RecoveryWarning = "按原目录恢复运行，未重新初始化账户。旧运行包未持久化独立 AccountSK，请核对隐私状态，不要重复 CreateAccount。"
			}
		}
	})
	message := ""
	status, running := "running", 0
	for _, node := range nodes {
		if node.Status == "running" {
			running++
		}
	}
	if resumeErr != nil {
		message = "部分节点尚未恢复，请查看节点错误并重试恢复：" + resumeErr.Error()
	}
	if running == 0 {
		status = "interrupted"
		message = "尚无节点完成恢复，请查看节点错误并重试继续实验。"
		if resumeErr != nil {
			message += " " + resumeErr.Error()
		}
	}
	if err := a.setExperiment(exp.ID, status, message, nodes, ""); err != nil {
		a.emit(exp.ID, "error", "lifecycle", "恢复执行结束但状态保存失败，请修复存储后核验，勿重新部署", nil)
		return
	}
	a.emit(exp.ID, "info", "lifecycle", "原实验恢复检查完成；运行包和原目录保留，未重发交易", map[string]any{"artifactSha": exp.ArtifactSHA})
	if running > 0 {
		go a.monitor(exp.ID)
	}
}

func (a *API) completeExperimentStop(experimentID string, results []orchestrator.StopResult, stopErr error, deploymentErr string) error {
	return a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			e := &s.Experiments[i]
			if e.ID != experimentID {
				continue
			}
			byID := map[string]orchestrator.StopResult{}
			for _, result := range results {
				byID[result.NodeID] = result
			}
			allStopped := stopErr == nil && len(e.Nodes) > 0
			for j := range e.Nodes {
				n := &e.Nodes[j]
				result, ok := byID[n.ID]
				n.Mining = nil
				if ok && result.Status == "stopped" {
					n.Status, n.Peers, n.StateError = "stopped", 0, ""
				} else {
					allStopped = false
					n.Status, n.StateError = "unknown", "未确认节点退出，请检查服务器后重试停止。"
					if result.Error != "" {
						n.StateError = result.Error
					}
				}
			}
			// A legacy partial deployment may have no stored node manifest.
			// Verified StopNodes results still provide an auditable outcome.
			if len(e.Nodes) == 0 && len(results) > 0 && stopErr == nil {
				allStopped = true
				for _, r := range results {
					if r.Status != "stopped" {
						allStopped = false
					}
				}
			}
			e.Status, e.Error = "stopped", deploymentErr
			if allStopped {
				e.FinishedAt = time.Now()
				if deploymentErr != "" {
					e.Status = "failed"
				}
			} else {
				e.Status = "stop-failed"
				e.Error += " 停止未完全确认；请检查节点详情并重试。"
				if stopErr != nil {
					e.Error += " " + stopErr.Error()
				}
			}
			return nil
		}
		return os.ErrNotExist
	})
}
