package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pfap/lab/internal/model"
)

// HostGroup is user-supplied topology metadata, never a command or SSH target.
// Unknown physical placement stays unknown; it is not inferred from an IP.
func normalizeHostGroup(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 80 {
		return "", errors.New("宿主机名称最多 80 个字符")
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return "", errors.New("宿主机名称不能包含控制字符或不可见格式字符")
		}
	}
	return value, nil
}

func (a *API) batchServerHostGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var request struct {
		IDs       []string `json:"ids"`
		HostGroup *string  `json:"hostGroup"`
	}
	if err := decode(r, &request); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if request.HostGroup == nil || len(request.IDs) < 1 || len(request.IDs) > 100 {
		fail(w, http.StatusBadRequest, errors.New("请选择 1–100 台服务器并明确填写宿主机名称；空字符串表示清除归属"))
		return
	}
	group, err := normalizeHostGroup(*request.HostGroup)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	selected := make(map[string]bool, len(request.IDs))
	for _, sid := range request.IDs {
		if sid == "" || selected[sid] {
			fail(w, http.StatusBadRequest, errors.New("服务器 ID 不能为空或重复"))
			return
		}
		selected[sid] = true
	}
	err = a.store.Update(func(s *model.State) error {
		found := 0
		for _, server := range s.Servers {
			if !selected[server.ID] {
				continue
			}
			found++
			if _, busy := a.serverMaintenance.Load(server.ID); busy {
				return fmt.Errorf("%s 正在执行磁盘维护，请稍后设置宿主机", server.Name)
			}
		}
		if found != len(selected) {
			return errors.New("所选服务器已变化或不存在，请刷新后重试")
		}
		for i := range s.Servers {
			if selected[s.Servers[i].ID] {
				s.Servers[i].HostGroup = group
			}
		}
		// Recording the metadata change must not recompute existing miner roles.
		s.Events = append(s.Events, model.Event{ID: id("evt"), Level: "info", Kind: "server-host-group", Message: "已更新服务器宿主机归属，现有矿工角色未改变", At: time.Now(), Fields: map[string]any{"serverIds": request.IDs, "hostGroup": group}})
		return nil
	})
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"status": "updated", "updated": len(selected), "hostGroup": group})
}
