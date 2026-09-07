package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/pfap/lab/internal/model"
)

type networkAddress struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
	Prefix    int    `json:"prefix"`
	State     string `json:"state"`
}

func parseNetworkAddresses(raw string) ([]networkAddress, error) {
	var interfaces []struct {
		Name      string   `json:"ifname"`
		State     string   `json:"operstate"`
		Flags     []string `json:"flags"`
		Addresses []struct {
			Family string `json:"family"`
			Local  string `json:"local"`
			Prefix int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(raw), &interfaces); err != nil {
		return nil, errors.New("无法解析网卡信息；服务器需要支持 ip -j address")
	}
	result := []networkAddress{}
	for _, iface := range interfaces {
		up := false
		for _, flag := range iface.Flags {
			if flag == "UP" {
				up = true
			}
		}
		if !up || iface.State == "DOWN" {
			continue
		}
		for _, address := range iface.Addresses {
			ip := net.ParseIP(address.Local)
			if address.Family != "inet" || ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() || address.Prefix < 0 || address.Prefix > 32 {
				continue
			}
			result = append(result, networkAddress{Interface: iface.Name, Address: ip.String(), Prefix: address.Prefix, State: iface.State})
		}
	}
	return result, nil
}

func (a *API) serverNetwork(w http.ResponseWriter, r *http.Request, server model.Server) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	raw, err := a.orch.Remote.Run(ctx, server, "ip -j address show\n")
	if err != nil {
		fail(w, http.StatusBadGateway, errors.New("读取网卡失败，请检查 SSH 信任、连接及 ip 命令；也可手动填写 P2P 地址"))
		return
	}
	addresses, err := parseNetworkAddresses(raw)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"addresses": addresses, "p2pHost": server.P2PHost, "checkedAt": time.Now()})
}
