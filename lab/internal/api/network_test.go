package api

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/pfap/lab/internal/model"
)

func TestNetworkAddressesAreExplicitUsableIPv4Choices(t *testing.T) {
	raw := `[
{"ifname":"lo","flags":["UP"],"addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8}]},
{"ifname":"ens18","operstate":"UP","flags":["UP"],"addr_info":[{"family":"inet","local":"192.168.50.219","prefixlen":24},{"family":"inet6","local":"2001:db8::1","prefixlen":64}]},
{"ifname":"ens19","operstate":"UP","flags":["UP"],"addr_info":[{"family":"inet","local":"100.99.0.99","prefixlen":16}]},
{"ifname":"down","flags":[],"addr_info":[{"family":"inet","local":"10.0.0.1","prefixlen":24}]},
{"ifname":"invalid","flags":["UP"],"addr_info":[{"family":"inet","local":"169.254.1.1","prefixlen":16},{"family":"inet","local":"0.0.0.0","prefixlen":0},{"family":"inet","local":"bad","prefixlen":24}]}
]`
	got, err := parseNetworkAddresses(raw)
	want := []networkAddress{{Interface: "ens18", Address: "192.168.50.219", Prefix: 24, State: "UP"}, {Interface: "ens19", Address: "100.99.0.99", Prefix: 16, State: "UP"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := parseNetworkAddresses("unsupported ip output"); err == nil {
		t.Fatal("invalid output accepted")
	}
}

func TestNetworkDiscoveryMethodAndActiveAddressProtection(t *testing.T) {
	a := hostGroupTestAPI(t)
	before := recoveryTestState(a)
	if r := groupRequest(a, http.MethodPost, "/api/servers/srv-a/network", nil); r.Code != http.StatusMethodNotAllowed {
		t.Fatal(r.Code)
	}
	body := before.Servers[0]
	body.P2PHost = "100.99.0.99"
	if r := groupRequest(a, http.MethodPut, "/api/servers/srv-a", body); r.Code != http.StatusConflict {
		t.Fatalf("active edit %d", r.Code)
	}
	if !reflect.DeepEqual(before, recoveryTestState(a)) {
		t.Fatal("active edit changed state")
	}
	if err := a.store.Update(func(s *model.State) error { s.Experiments[0].Status = "stopped"; return nil }); err != nil {
		t.Fatal(err)
	}
	if r := groupRequest(a, http.MethodPut, "/api/servers/srv-a", body); r.Code != http.StatusOK {
		t.Fatalf("stopped edit %d %s", r.Code, r.Body.String())
	}
	after := recoveryTestState(a)
	if after.Servers[0].P2PHost != "100.99.0.99" || after.Servers[0].Host != before.Servers[0].Host || !reflect.DeepEqual(after.Experiments[0].Nodes, before.Experiments[0].Nodes) {
		t.Fatal("address edit changed management address or nodes")
	}
}
