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

// Migrate saved configuration without issuing commands or changing a running
// experiment's original single-miner baseline. Observations must be resampled.
func migrateMiningState(s *model.State) {
	for i := range s.Experiments {
		e := &s.Experiments[i]
		legacy := e.MinerCount == 0
		if legacy {
			e.MinerCount = 1
		}
		if e.MiningStatus == "updating" {
			e.MiningStatus = "failed"
			e.MiningError = "控制器重启中断了矿工配置，可能仅部分生效；请检查实际挖矿状态并重试。"
		}
		for j := range e.Nodes {
			if legacy {
				e.Nodes[j].IsMiner = e.Nodes[j].Index == 1
			}
			e.Nodes[j].Mining = nil
		}
	}
}

func (a *API) updateMiners(w http.ResponseWriter, r *http.Request, experimentID string) {
	var request struct {
		MinerCount int `json:"minerCount"`
	}
	if err := decode(r, &request); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	exp, servers, err := a.beginMiningUpdate(experimentID, request.MinerCount)
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, os.ErrNotExist) {
			code = http.StatusNotFound
		}
		fail(w, code, err)
		return
	}
	a.emit(exp.ID, "info", "mining", "矿工目标配置已保存", map[string]any{"minerCount": exp.MinerCount, "status": exp.MiningStatus})
	if exp.Status == "running" {
		go a.runMiningUpdate(exp, servers)
		jsonOut(w, http.StatusAccepted, exp)
		return
	}
	jsonOut(w, http.StatusOK, exp)
}

func (a *API) beginMiningUpdate(experimentID string, count int) (model.Experiment, map[string]model.Server, error) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	var exp model.Experiment
	servers := map[string]model.Server{}
	err := a.store.Update(func(s *model.State) error {
		for _, server := range s.Servers {
			servers[server.ID] = server
		}
		for i := range s.Experiments {
			e := &s.Experiments[i]
			if e.ID != experimentID {
				continue
			}
			if e.Status != "draft" && e.Status != "stopped" && e.Status != "running" {
				return errors.New("只能调整草稿、已停止或运行中的实验；请先等待当前操作完成")
			}
			if e.MiningStatus == "updating" {
				return errors.New("矿工配置正在应用，请勿重复操作")
			}
			total := len(e.Nodes)
			if total == 0 {
				for _, p := range e.Placements {
					total += p.Count
				}
			}
			if count < 1 || count > total {
				return fmt.Errorf("矿工数量必须在 1 到 %d 之间", total)
			}
			nodes := append([]model.Node(nil), e.Nodes...)
			if len(nodes) > 0 {
				if err := model.AssignMiners(nodes, count); err != nil {
					return err
				}
			}
			if e.Status == "running" {
				available := 0
				for j, n := range nodes {
					if n.Status == "recovering" {
						return errors.New("节点正在恢复，请等待恢复完成后再调整矿工")
					}
					if _, ok := servers[n.ServerID]; !ok {
						return fmt.Errorf("%s 关联的服务器配置不存在", n.Name)
					}
					if n.Status != "running" && n.IsMiner != model.NodeIsMiner(*e, e.Nodes[j]) {
						return fmt.Errorf("%s 不可达，无法确认其矿工角色变更；请先恢复该节点", n.Name)
					}
					if n.IsMiner && n.Status == "running" {
						available++
					}
				}
				if available == 0 {
					return errors.New("目标矿工中没有可达节点；请先恢复至少一个目标矿工")
				}
			}
			// Validate everything before mutating: Store.Update is not a rollback
			// transaction when its callback returns an error.
			e.MinerCount, e.Nodes = count, nodes
			e.MiningError, e.MiningStatus = "", ""
			e.MiningUpdatedAt = time.Now()
			if e.Status == "running" {
				e.MiningStatus = "updating"
			}
			exp = *e
			exp.Nodes = append([]model.Node(nil), nodes...)
			return nil
		}
		return os.ErrNotExist
	})
	return exp, servers, err
}

func (a *API) runMiningUpdate(exp model.Experiment, servers map[string]model.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var failures []error
	apply := func(n model.Node, enabled bool) {
		err := a.orch.SetMining(ctx, exp, n, servers[n.ServerID], enabled)
		var observed *bool
		if err == nil {
			value := enabled
			observed = &value
		} else {
			failures = append(failures, err)
		}
		if updateErr := a.store.Update(func(s *model.State) error {
			for i := range s.Experiments {
				e := &s.Experiments[i]
				if e.ID == exp.ID {
					for j := range e.Nodes {
						if e.Nodes[j].ID == n.ID {
							e.Nodes[j].Mining = observed
						}
					}
				}
			}
			return nil
		}); updateErr != nil {
			failures = append(failures, updateErr)
		}
	}
	// Never stop an old miner before all reachable target miners have been
	// confirmed. Failure is explicitly partial, not a fictional atomic rollback.
	for _, n := range exp.Nodes {
		if n.IsMiner && n.Status == "running" {
			apply(n, true)
		}
	}
	if len(failures) == 0 {
		for _, n := range exp.Nodes {
			if !n.IsMiner && n.Status == "running" {
				apply(n, false)
			}
		}
	}
	result := errors.Join(failures...)
	status, message, level := "", "矿工配置已应用；实际挖矿数量以节点监控为准", "info"
	if result != nil {
		status, level = "failed", "error"
		message = "矿工配置未完全生效，已保留目标配置，请检查节点后重试：" + result.Error()
	}
	if err := a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			if s.Experiments[i].ID == exp.ID {
				e := &s.Experiments[i]
				e.MiningStatus, e.MiningError = status, ""
				if result != nil {
					e.MiningError = message
				}
				e.MiningUpdatedAt = time.Now()
			}
		}
		return nil
	}); err != nil {
		level, message = "error", "矿工操作结束，但保存结果失败："+err.Error()
	}
	a.emit(exp.ID, level, "mining", message, map[string]any{"minerCount": exp.MinerCount})
}
