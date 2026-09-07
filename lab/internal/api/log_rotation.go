package api

import (
	"context"
	"sync"
	"time"

	"github.com/pfap/lab/internal/model"
)

func (a *API) monitorLogRotation() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		var ids []string
		a.store.View(func(s model.State) {
			for _, exp := range s.Experiments {
				for _, node := range exp.Nodes {
					ids = append(ids, node.ID)
				}
			}
		})
		for _, nodeID := range ids {
			a.rotateIdleNodeLog(nodeID)
		}
	}
}

func rotationCandidate(s model.State, nodeID string) (model.Experiment, model.Node, model.Server, bool) {
	if transactionNodesBusy(s.Transactions, nodeID, "") {
		return model.Experiment{}, model.Node{}, model.Server{}, false
	}
	for _, exp := range s.Experiments {
		if exp.Status != "running" && exp.Status != "stopped" && exp.Status != "failed" && exp.Status != "interrupted" && exp.Status != "stop-failed" {
			continue
		}
		if exp.MiningStatus == "updating" {
			continue
		}
		for _, node := range exp.Nodes {
			if node.ID != nodeID || node.Status == "recovering" {
				continue
			}
			for _, server := range s.Servers {
				if server.ID == node.ServerID && server.Status != "error" {
					return exp, node, server, true
				}
			}
		}
	}
	return model.Experiment{}, model.Node{}, model.Server{}, false
}

func (a *API) tryLogRotationAdmission() bool {
	if a.stopWaiters.Load() > 0 {
		return false
	}
	if !a.lifecycleMu.TryLock() {
		return false
	}
	if a.stopWaiters.Load() > 0 {
		a.lifecycleMu.Unlock()
		return false
	}
	return true
}

func (a *API) rotateIdleNodeLog(nodeID string) {
	// Same admission ordering as recovery; yield to queued user stops.
	if !a.tryLogRotationAdmission() {
		return
	}
	defer a.lifecycleMu.Unlock()
	raw, _ := a.nodeLocks.LoadOrStore(nodeID, &sync.Mutex{})
	lock := raw.(*sync.Mutex)
	if !lock.TryLock() {
		return
	}
	defer lock.Unlock()
	var exp model.Experiment
	var node model.Node
	var server model.Server
	var eligible bool
	a.store.View(func(s model.State) { exp, node, server, eligible = rotationCandidate(s, nodeID) })
	if !eligible {
		return
	}
	a.serverMaintenance.Store(server.ID, true)
	defer a.serverMaintenance.Delete(server.ID)
	// Serialize this snapshot with edits already inside the store callback.
	a.store.View(func(s model.State) {
		_, _, current, ok := rotationCandidate(s, nodeID)
		eligible = ok && sameDiskServer(server, current)
	})
	if !eligible {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rotated, err := a.orch.RotateNodeLog(ctx, exp, node, server)
	if err != nil {
		a.emit(exp.ID, "warning", "log-rotation", "节点日志归档失败："+err.Error(), map[string]any{"nodeId": node.ID, "serverId": server.ID})
	} else if rotated {
		a.emit(exp.ID, "info", "log-rotation", "节点日志已归档，保留最近 3 份历史日志", map[string]any{"nodeId": node.ID, "serverId": server.ID})
	}
}
