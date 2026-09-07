package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/orchestrator"
)

const diskPlanLifetime = 10 * time.Minute

type diskPlan struct {
	Server    model.Server
	CheckedAt time.Time
	Targets   []orchestrator.CacheTarget
}

func sameDiskServer(a, b model.Server) bool {
	return a.ID == b.ID && a.Host == b.Host && a.Port == b.Port && a.User == b.User && a.WorkDir == b.WorkDir && a.IdentityFile == b.IdentityFile && a.KnownHostsFile == b.KnownHostsFile
}

func stoppedCacheExperiment(exp model.Experiment, placementID string) bool {
	if (exp.Status != "stopped" && exp.Status != "failed") || exp.MiningStatus == "updating" || len(exp.Nodes) == 0 {
		return false
	}
	foundNode, foundPlacement := false, false
	for _, node := range exp.Nodes {
		if node.Status != "stopped" {
			return false
		}
		foundNode = foundNode || node.ServerID == placementID
	}
	for _, p := range exp.Placements {
		foundPlacement = foundPlacement || (p.ServerID == placementID && p.Count > 0)
	}
	return foundNode && foundPlacement
}

// Revalidate stored identities, not client-supplied byte counts or path names.
// Historical placement IDs can differ from the currently registered worker ID.
func validateDiskTargets(state model.State, server model.Server, plan diskPlan, requested []orchestrator.CacheTarget, now time.Time) ([]orchestrator.CacheTarget, error) {
	if len(requested) < 1 || len(requested) > 100 {
		return nil, errors.New("请选择 1–100 个已检查的缓存目录")
	}
	if now.Before(plan.CheckedAt) || now.Sub(plan.CheckedAt) > diskPlanLifetime || !sameDiskServer(server, plan.Server) {
		return nil, errors.New("磁盘检查已过期或服务器配置已更改，请重新检查")
	}
	foundServer := false
	for _, current := range state.Servers {
		foundServer = foundServer || sameDiskServer(current, server)
	}
	if !foundServer {
		return nil, errors.New("服务器配置已更改，请重新检查")
	}
	selected := make([]orchestrator.CacheTarget, 0, len(requested))
	seen := make(map[string]bool)
	for _, req := range requested {
		if seen[req.Path] {
			return nil, errors.New("清理目录重复")
		}
		seen[req.Path] = true
		var target orchestrator.CacheTarget
		for _, cached := range plan.Targets {
			if cached.Path == req.Path && cached.ExperimentID == req.ExperimentID && cached.ServerID == req.ServerID && cached.Fingerprint == req.Fingerprint {
				target = cached
				break
			}
		}
		if target.Path == "" {
			return nil, errors.New("清理目标未经过检查或指纹不匹配，请重新检查")
		}
		stopped := false
		for _, exp := range state.Experiments {
			if exp.ID == target.ExperimentID {
				stopped = stoppedCacheExperiment(exp, target.ServerID)
			}
		}
		if !stopped {
			return nil, errors.New("仅可清理所有节点均已确认停止的已记录实验")
		}
		for _, tx := range state.Transactions {
			if tx.ExperimentID == target.ExperimentID && activeTransaction(tx.Status) {
				return nil, errors.New("实验仍有执行中或待核验交易，不能清理")
			}
		}
		selected = append(selected, target)
	}
	return selected, nil
}

func (a *API) serverDiskAction(w http.ResponseWriter, r *http.Request, server model.Server, parts []string) {
	if len(parts) == 4 && r.Method == http.MethodGet {
		a.inspectServerDisk(w, r, server)
		return
	}
	if len(parts) == 5 && parts[4] == "cleanup" && r.Method == http.MethodPost {
		a.cleanupServerDisk(w, r, server)
		return
	}
	fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
}

func (a *API) inspectServerDisk(w http.ResponseWriter, r *http.Request, server model.Server) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	disk, err := a.orch.DiskUsage(ctx, server)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	var experiments []model.Experiment
	a.store.View(func(s model.State) {
		for _, exp := range s.Experiments {
			busy := false
			for _, tx := range s.Transactions {
				busy = busy || (tx.ExperimentID == exp.ID && activeTransaction(tx.Status))
			}
			if !busy && exp.MiningStatus != "updating" {
				experiments = append(experiments, exp)
			}
		}
	})
	targets, err := a.orch.ScanDAGCaches(ctx, server, experiments)
	if err != nil {
		a.diskPlans.Delete(server.ID)
		fail(w, http.StatusBadGateway, err)
		return
	}
	plan := diskPlan{Server: server, CheckedAt: time.Now(), Targets: targets}
	a.diskPlans.Store(server.ID, plan)
	jsonOut(w, http.StatusOK, map[string]any{
		"serverId": server.ID, "checkedAt": plan.CheckedAt, "disk": disk, "candidates": targets,
		"logPolicy": map[string]any{"enabled": true, "maxBytes": orchestrator.LogMaxBytes, "retainFiles": orchestrator.LogRetainFiles},
	})
}

type diskCleanupResult struct {
	ExperimentID string `json:"experimentId"`
	Path         string `json:"path"`
	FreedBytes   int64  `json:"freedBytes"`
	RemovedFiles int    `json:"removedFiles"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}

func (a *API) cleanupServerDisk(w http.ResponseWriter, r *http.Request, server model.Server) {
	var request struct {
		Targets []orchestrator.CacheTarget `json:"targets"`
	}
	if err := decode(r, &request); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	// Held through unlink, excluding new deployment/recovery/miner admission.
	if !a.lifecycleMu.TryLock() {
		fail(w, http.StatusConflict, errors.New("正在执行实验操作或磁盘维护，请稍后重试"))
		return
	}
	defer a.lifecycleMu.Unlock()
	a.serverMaintenance.Store(server.ID, true)
	defer a.serverMaintenance.Delete(server.ID)
	raw, ok := a.diskPlans.Load(server.ID)
	if !ok {
		fail(w, http.StatusConflict, errors.New("请先检查磁盘并选择缓存目录"))
		return
	}
	var targets []orchestrator.CacheTarget
	// Persist the intent before the first deletion. Failed storage prevents action.
	err := a.store.Update(func(s *model.State) error {
		var validationErr error
		targets, validationErr = validateDiskTargets(*s, server, raw.(diskPlan), request.Targets, time.Now())
		if validationErr != nil {
			return validationErr
		}
		s.Events = append(s.Events, model.Event{ID: id("evt"), Kind: "disk-cleanup", Level: "info", Message: "已确认清理停止实验的 DAG 缓存", Fields: map[string]any{"serverId": server.ID, "targets": targets}, At: time.Now()})
		return nil
	})
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	a.diskPlans.Delete(server.ID) // One-shot review, including partial failures.
	// Do not abandon accounting just because the browser disconnects.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	results := make([]diskCleanupResult, 0, len(targets))
	var freed int64
	for _, target := range targets {
		result, cleanupErr := a.orch.CleanupDAGCache(ctx, server, target)
		if cleanupErr != nil && result.Error == "" {
			result.Error = cleanupErr.Error()
		}
		freed += result.FreedBytes
		results = append(results, diskCleanupResult{ExperimentID: target.ExperimentID, Path: target.Path, FreedBytes: result.FreedBytes, RemovedFiles: result.RemovedFiles, Status: result.Status, Error: result.Error})
	}
	response := map[string]any{"results": results, "freedBytes": freed}
	if err := a.store.Update(func(s *model.State) error {
		s.Events = append(s.Events, model.Event{ID: id("evt"), Kind: "disk-cleanup", Level: "info", Message: "DAG 缓存清理已结束（按文件实际占用估算释放空间）", Fields: map[string]any{"serverId": server.ID, "results": results, "freedBytes": freed}, At: time.Now()})
		return nil
	}); err != nil {
		response["storageWarning"] = fmt.Sprintf("清理已执行，但结果持久化失败：%v", err)
	}
	jsonOut(w, http.StatusOK, response)
}
