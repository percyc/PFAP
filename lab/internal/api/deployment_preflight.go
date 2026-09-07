package api

import (
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/pfap/lab/internal/model"
)

// restoreDeploymentDraft only removes the pristine manifest pinned for this
// fresh admission. A preflight failure cannot authorize clearing reused or
// subsequently changed experiment state, even though it made no worker writes.
func (a *API) restoreDeploymentDraft(admitted model.Experiment, preflightErr error) error {
	if admitted.Status != "deploying" || admitted.ArtifactSHA == "" || !admitted.StartedAt.IsZero() || len(admitted.Nodes) == 0 {
		return errors.New("部署预检结果不属于未启动的新实验，保留现有状态")
	}
	pristine, err := plannedNodes(admitted)
	// The admitted automatic plan can have been spread using host-group
	// metadata, which this identity-only helper intentionally does not fetch.
	// Keep the exact admitted roles; never recompute them from mutable metadata.
	if err == nil && len(pristine) == len(admitted.Nodes) {
		for i := range pristine {
			pristine[i].IsMiner = admitted.Nodes[i].IsMiner
		}
	}
	if err != nil || !reflect.DeepEqual(admitted.Nodes, pristine) {
		return errors.New("部署节点清单不是本次新规划的清单，保留现有状态")
	}
	return a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			e := &s.Experiments[i]
			if e.ID != admitted.ID {
				continue
			}
			if e.Status != "deploying" || e.ArtifactSHA != admitted.ArtifactSHA || !e.StartedAt.IsZero() || !reflect.DeepEqual(e.Nodes, admitted.Nodes) {
				return errors.New("部署状态或原节点清单已变化，保留现有状态")
			}
			e.Status, e.ArtifactSHA, e.Nodes = "draft", "", nil
			e.Error = fmt.Sprintf("部署预检未通过，尚未写入 worker；实验已回到草稿，排除问题后可重试。原因：%v", preflightErr)
			return nil
		}
		return os.ErrNotExist
	})
}
