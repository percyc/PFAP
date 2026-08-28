package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/orchestrator"
	"github.com/pfap/lab/internal/store"
)

type API struct {
	store       *store.Store
	orch        orchestrator.Orchestrator
	subscribers map[chan model.Event]struct{}
	mu          sync.Mutex
	nodeLocks   sync.Map
}

func New(s *store.Store) *API {
	a := &API{store: s, subscribers: map[chan model.Event]struct{}{}}
	var running []string
	_ = s.Update(func(state *model.State) error {
		for i := range state.Experiments {
			if state.Experiments[i].Status == "running" {
				state.Experiments[i].FinishedAt = time.Time{}
			}
		}
		// Associate transactions created by older Lab versions with their
		// workload so historical automatic runs gain route-level traceability.
		for _, workload := range state.Workloads {
			start := workload.StartedAt
			if start.IsZero() {
				start = workload.CreatedAt
			}
			end := start.Add(time.Duration(workload.DurationSeconds)*time.Second + time.Second)
			sequence := 0
			for i := range state.Transactions {
				tx := &state.Transactions[i]
				if tx.WorkloadID != "" || tx.ExperimentID != workload.ExperimentID || tx.Type != workload.Type || tx.SubmittedAt.Before(start) || tx.SubmittedAt.After(end) {
					continue
				}
				sequence++
				tx.WorkloadID = workload.ID
				tx.Sequence = sequence
			}
		}
		for i := range state.Workloads {
			workload := &state.Workloads[i]
			failed := 0
			for _, tx := range state.Transactions {
				if tx.WorkloadID == workload.ID && (tx.Status == "failed" || tx.Status == "timeout") {
					failed++
				}
			}
			if failed > 0 && workload.Status == "completed" {
				workload.Status = "completed-with-errors"
				workload.Error = fmt.Sprintf("%d of %d transactions failed", failed, workload.Submitted)
			}
		}
		return nil
	})
	s.View(func(state model.State) {
		for _, e := range state.Experiments {
			if e.Status == "running" {
				running = append(running, e.ID)
			}
		}
	})
	for _, experimentID := range running {
		go a.monitor(experimentID)
	}
	go a.backfillTransactionTimings()
	return a
}

func (a *API) backfillTransactionTimings() {
	var txs []model.Transaction
	experiments := map[string]model.Experiment{}
	servers := map[string]model.Server{}
	a.store.View(func(s model.State) {
		for _, e := range s.Experiments {
			experiments[e.ID] = e
		}
		for _, server := range s.Servers {
			servers[server.ID] = server
		}
		for _, tx := range s.Transactions {
			if tx.Status == "confirmed" && tx.Hash != "" && tx.Type != "public" && tx.Type != "transfer" {
				txs = append(txs, tx)
			}
		}
	})
	if len(txs) > 200 {
		txs = txs[len(txs)-200:]
	}
	for _, tx := range txs {
		exp, ok := experiments[tx.ExperimentID]
		if !ok {
			continue
		}
		var node model.Node
		for _, candidate := range exp.Nodes {
			if candidate.ID == tx.FromNode {
				node = candidate
				break
			}
		}
		server, ok := servers[node.ServerID]
		if node.ID == "" || !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		proofUs, verifyUs, err := a.orch.TransactionTimings(ctx, exp, node, server, tx.Hash)
		cancel()
		if err != nil {
			continue
		}
		_ = a.store.Update(func(s *model.State) error {
			for i := range s.Transactions {
				if s.Transactions[i].ID == tx.ID {
					s.Transactions[i].ProofDurationUs = proofUs
					s.Transactions[i].VerifyDurationUs = verifyUs
					s.Transactions[i].ProofDurationMs = proofUs / 1000
					s.Transactions[i].VerifyDurationMs = verifyUs / 1000
				}
			}
			return nil
		})
	}
}
func id(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}
func fail(w http.ResponseWriter, status int, err error) {
	jsonOut(w, status, map[string]string{"error": err.Error()})
}

func (a *API) emit(exp, level, kind, message string, fields map[string]any) {
	e := model.Event{ID: id("evt"), ExperimentID: exp, Level: level, Kind: kind, Message: message, Fields: fields, At: time.Now()}
	_ = a.store.Update(func(s *model.State) error { s.Events = append(s.Events, e); return nil })
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.subscribers {
		select {
		case ch <- e:
		default:
		}
	}
}

func (a *API) Handler(static http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path == "/api/health" {
			jsonOut(w, 200, map[string]any{"status": "ok", "time": time.Now()})
			return
		}
		if r.URL.Path == "/api/state" && r.Method == "GET" {
			a.store.View(func(s model.State) { jsonOut(w, 200, s) })
			return
		}
		if r.URL.Path == "/api/events/stream" {
			a.stream(w, r)
			return
		}
		if r.URL.Path == "/api/servers" {
			a.servers(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/servers/") {
			a.serverAction(w, r)
			return
		}
		if r.URL.Path == "/api/experiments" {
			a.experiments(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/experiments/") {
			a.experimentAction(w, r)
			return
		}
		if r.URL.Path == "/api/transactions" {
			a.transactions(w, r)
			return
		}
		if r.URL.Path == "/api/workloads" {
			a.workloads(w, r)
			return
		}
		if r.URL.Path == "/api/console/execute" {
			a.executeConsole(w, r)
			return
		}
		if r.URL.Path == "/api/metrics" && r.Method == "GET" {
			a.metrics(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/transactions/") {
			a.transactionAction(w, r)
			return
		}
		static.ServeHTTP(w, r)
	})
}

func (a *API) executeConsole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var request struct {
		ExperimentID string `json:"experimentId"`
		NodeID       string `json:"nodeId"`
		Command      string `json:"command"`
	}
	if err := decode(r, &request); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	request.Command = strings.TrimSpace(request.Command)
	if request.ExperimentID == "" || request.NodeID == "" || request.Command == "" {
		fail(w, http.StatusBadRequest, errors.New("experimentId, nodeId and command are required"))
		return
	}
	if len(request.Command) > 16*1024 {
		fail(w, http.StatusBadRequest, errors.New("command exceeds 16 KiB limit"))
		return
	}

	var exp model.Experiment
	var node model.Node
	var server model.Server
	a.store.View(func(s model.State) {
		for _, candidate := range s.Experiments {
			if candidate.ID == request.ExperimentID {
				exp = candidate
				for _, candidateNode := range candidate.Nodes {
					if candidateNode.ID == request.NodeID {
						node = candidateNode
					}
				}
			}
		}
		for _, candidate := range s.Servers {
			if candidate.ID == node.ServerID {
				server = candidate
			}
		}
	})
	if exp.ID == "" || node.ID == "" || server.ID == "" {
		fail(w, http.StatusNotFound, errors.New("experiment, node or server not found"))
		return
	}
	if exp.Status != "running" || node.Status != "running" {
		fail(w, http.StatusConflict, errors.New("experiment and node must be running"))
		return
	}

	lockAny, _ := a.nodeLocks.LoadOrStore(node.ID, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	started := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	output, err := a.orch.Attach(ctx, exp, node, server, request.Command)
	result := map[string]any{"experimentId": exp.ID, "nodeId": node.ID, "nodeName": node.Name, "output": output, "durationMs": time.Since(started).Milliseconds()}
	if err != nil {
		result["error"] = err.Error()
		jsonOut(w, http.StatusUnprocessableEntity, result)
		return
	}
	jsonOut(w, http.StatusOK, result)
}

func (a *API) servers(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.store.View(func(s model.State) { jsonOut(w, 200, s.Servers) })
		return
	}
	if r.Method != "POST" {
		fail(w, 405, errors.New("method not allowed"))
		return
	}
	var v model.Server
	if err := decode(r, &v); err != nil {
		fail(w, 400, err)
		return
	}
	if v.Name == "" || v.Host == "" || v.User == "" {
		fail(w, 400, errors.New("name, host and user are required"))
		return
	}
	if v.Port == 0 {
		v.Port = 22
	}
	if v.WorkDir == "" {
		v.WorkDir = "/opt/pfap-lab"
	}
	v.ID = id("srv")
	v.Status = "unknown"
	v.CreatedAt = time.Now()
	if err := a.store.Update(func(s *model.State) error { s.Servers = append(s.Servers, v); return nil }); err != nil {
		fail(w, 500, err)
		return
	}
	jsonOut(w, 201, v)
}

func (a *API) serverAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		fail(w, 404, errors.New("not found"))
		return
	}
	sid := parts[2]
	var server model.Server
	found := false
	a.store.View(func(s model.State) {
		for _, x := range s.Servers {
			if x.ID == sid {
				server = x
				found = true
			}
		}
	})
	if !found {
		fail(w, 404, errors.New("server not found"))
		return
	}
	if len(parts) == 4 && parts[3] == "check" && r.Method == "POST" {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		out, err := a.orch.Check(ctx, server)
		status := "online"
		if err != nil {
			status = "error"
			out = err.Error()
		}
		_ = a.store.Update(func(s *model.State) error {
			for i := range s.Servers {
				if s.Servers[i].ID == sid {
					s.Servers[i].Status = status
					s.Servers[i].SystemInfo = out
					s.Servers[i].LastCheck = time.Now()
				}
			}
			return nil
		})
		if err != nil {
			fail(w, 502, err)
			return
		}
		jsonOut(w, 200, map[string]string{"status": status, "info": out})
		return
	}
	if len(parts) == 3 && r.Method == "DELETE" {
		err := a.store.Update(func(s *model.State) error {
			for _, e := range s.Experiments {
				for _, p := range e.Placements {
					if p.ServerID == sid {
						return errors.New("server is referenced by an experiment")
					}
				}
			}
			for i, x := range s.Servers {
				if x.ID == sid {
					s.Servers = append(s.Servers[:i], s.Servers[i+1:]...)
					return nil
				}
			}
			return nil
		})
		if err != nil {
			fail(w, 409, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	fail(w, 404, errors.New("not found"))
}

func (a *API) experiments(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.store.View(func(s model.State) { jsonOut(w, 200, s.Experiments) })
		return
	}
	if r.Method != "POST" {
		fail(w, 405, errors.New("method not allowed"))
		return
	}
	var e model.Experiment
	if err := decode(r, &e); err != nil {
		fail(w, 400, err)
		return
	}
	if e.Name == "" || len(e.Placements) == 0 {
		fail(w, 400, errors.New("name and placements are required"))
		return
	}
	knownServers := map[string]bool{}
	var activeExperiments []model.Experiment
	a.store.View(func(s model.State) {
		for _, x := range s.Servers {
			knownServers[x.ID] = true
		}
		for _, x := range s.Experiments {
			if x.Status == "running" || x.Status == "deploying" || x.Status == "stopping" {
				activeExperiments = append(activeExperiments, x)
			}
		}
	})
	seen := map[string]bool{}
	totalNodes := 0
	for _, p := range e.Placements {
		if !knownServers[p.ServerID] {
			fail(w, 400, fmt.Errorf("unknown server %s", p.ServerID))
			return
		}
		if seen[p.ServerID] {
			fail(w, 400, fmt.Errorf("duplicate server %s", p.ServerID))
			return
		}
		if p.Count < 1 {
			fail(w, 400, errors.New("node count must be positive"))
			return
		}
		seen[p.ServerID] = true
		totalNodes += p.Count
	}
	if totalNodes > 100 {
		fail(w, 400, errors.New("an experiment is limited to 100 nodes"))
		return
	}
	if e.NetworkID == 0 {
		e.NetworkID = 55661
	}
	if e.P2PPortBase == 0 {
		e.P2PPortBase = nextPortBase(30000, totalNodes, activeExperiments, false)
	}
	if e.RPCPortBase == 0 {
		e.RPCPortBase = nextPortBase(40000, totalNodes, activeExperiments, true)
	}
	if e.P2PPortBase < 1 || e.RPCPortBase < 1 || e.P2PPortBase+totalNodes > 65536 || e.RPCPortBase+totalNodes > 65536 {
		fail(w, 400, errors.New("node port range is invalid"))
		return
	}
	if e.Topology == "" {
		e.Topology = "full-mesh"
	}
	e.ID = id("exp")
	e.Status = "draft"
	e.CreatedAt = time.Now()
	if err := a.store.Update(func(s *model.State) error { s.Experiments = append(s.Experiments, e); return nil }); err != nil {
		fail(w, 500, err)
		return
	}
	jsonOut(w, 201, e)
}

func nextPortBase(start, count int, experiments []model.Experiment, rpc bool) int {
	base := start
	for {
		conflict := false
		for _, e := range experiments {
			other := e.P2PPortBase
			if rpc {
				other = e.RPCPortBase
			}
			otherCount := len(e.Nodes)
			if otherCount == 0 {
				for _, p := range e.Placements {
					otherCount += p.Count
				}
			}
			if base < other+otherCount && other < base+count {
				conflict = true
				base = other + ((otherCount+99)/100)*100
				break
			}
		}
		if !conflict {
			return base
		}
	}
}

func (a *API) experimentAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 6 && parts[3] == "nodes" && parts[5] == "state" && (r.Method == "GET" || r.Method == "POST") {
		a.nodeState(w, r, parts[2], parts[4])
		return
	}
	if len(parts) != 4 {
		fail(w, 404, errors.New("not found"))
		return
	}
	eid, action := parts[2], parts[3]
	if action == "report" && r.Method == "GET" {
		a.report(w, eid)
		return
	}
	if r.Method != "POST" {
		fail(w, 405, errors.New("method not allowed"))
		return
	}
	var exp model.Experiment
	servers := map[string]model.Server{}
	found := false
	a.store.View(func(s model.State) {
		for _, e := range s.Experiments {
			if e.ID == eid {
				exp = e
				found = true
			}
		}
		for _, x := range s.Servers {
			servers[x.ID] = x
		}
	})
	if !found {
		fail(w, 404, errors.New("experiment not found"))
		return
	}
	if (action == "deploy" || action == "start") && (exp.Status == "deploying" || exp.Status == "running") {
		fail(w, 409, errors.New("experiment is already active"))
		return
	}
	if (action == "deploy" || action == "start") && exp.Status == "failed" {
		total := 0
		for _, p := range exp.Placements {
			total += p.Count
		}
		var active []model.Experiment
		a.store.View(func(s model.State) {
			for _, e := range s.Experiments {
				if e.ID != exp.ID && (e.Status == "running" || e.Status == "deploying" || e.Status == "stopping") {
					active = append(active, e)
				}
			}
		})
		exp.P2PPortBase = nextPortBase(30000, total, active, false)
		exp.RPCPortBase = nextPortBase(40000, total, active, true)
		_ = a.store.Update(func(s *model.State) error {
			for i := range s.Experiments {
				if s.Experiments[i].ID == exp.ID {
					s.Experiments[i].P2PPortBase = exp.P2PPortBase
					s.Experiments[i].RPCPortBase = exp.RPCPortBase
					s.Experiments[i].Error = ""
				}
			}
			return nil
		})
	}
	switch action {
	case "deploy", "start":
		go a.deploy(eid, exp, servers)
		jsonOut(w, 202, map[string]string{"status": "deploying"})
	case "stop":
		go a.stop(eid, exp, servers)
		jsonOut(w, 202, map[string]string{"status": "stopping"})
	default:
		fail(w, 404, errors.New("unknown action"))
	}
}

func (a *API) nodeState(w http.ResponseWriter, r *http.Request, experimentID, nodeID string) {
	var exp model.Experiment
	var node model.Node
	var server model.Server
	found := false
	a.store.View(func(s model.State) {
		for _, e := range s.Experiments {
			if e.ID == experimentID {
				exp = e
				for _, n := range e.Nodes {
					if n.ID == nodeID {
						node = n
						found = true
					}
				}
			}
		}
		for _, x := range s.Servers {
			if x.ID == node.ServerID {
				server = x
			}
		}
	})
	if !found {
		fail(w, 404, errors.New("node not found"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := a.sampleNode(ctx, exp, node, server, "manual-query"); err != nil {
		fail(w, 502, err)
		return
	}
	a.store.View(func(s model.State) {
		for _, e := range s.Experiments {
			if e.ID == experimentID {
				for _, n := range e.Nodes {
					if n.ID == nodeID {
						jsonOut(w, 200, n)
						return
					}
				}
			}
		}
	})
}

func (a *API) report(w http.ResponseWriter, eid string) {
	var exp *model.Experiment
	var txs []model.Transaction
	var loads []model.Workload
	var events []model.Event
	a.store.View(func(s model.State) {
		for i := range s.Experiments {
			if s.Experiments[i].ID == eid {
				copy := s.Experiments[i]
				exp = &copy
			}
		}
		for _, t := range s.Transactions {
			if t.ExperimentID == eid {
				txs = append(txs, t)
			}
		}
		for _, x := range s.Workloads {
			if x.ExperimentID == eid {
				loads = append(loads, x)
			}
		}
		for _, e := range s.Events {
			if e.ExperimentID == eid {
				events = append(events, e)
			}
		}
	})
	if exp == nil {
		fail(w, 404, errors.New("experiment not found"))
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="pfap-`+eid+`.json"`)
	jsonOut(w, 200, map[string]any{"schemaVersion": 1, "generatedAt": time.Now(), "experiment": exp, "transactions": txs, "workloads": loads, "events": events})
}

func (a *API) setExperiment(id, status, errText string, nodes []model.Node, sha string) {
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			if s.Experiments[i].ID == id {
				s.Experiments[i].Status = status
				s.Experiments[i].Error = errText
				if nodes != nil {
					s.Experiments[i].Nodes = nodes
				}
				if sha != "" {
					s.Experiments[i].ArtifactSHA = sha
				}
				if status == "running" {
					s.Experiments[i].StartedAt = time.Now()
					s.Experiments[i].FinishedAt = time.Time{}
				}
				if status == "stopped" || status == "failed" {
					s.Experiments[i].FinishedAt = time.Now()
				}
				if status == "stopped" {
					for j := range s.Experiments[i].Nodes {
						s.Experiments[i].Nodes[j].Status = "stopped"
						s.Experiments[i].Nodes[j].Peers = 0
					}
				}
			}
		}
		return nil
	})
}
func (a *API) deploy(id string, e model.Experiment, servers map[string]model.Server) {
	a.setExperiment(id, "deploying", "", nil, "")
	a.emit(id, "info", "lifecycle", "deployment started", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	nodes, err := a.orch.Deploy(ctx, &e, servers, func(l, k, m string, f map[string]any) { a.emit(id, l, k, m, f) })
	if err != nil {
		a.setExperiment(id, "failed", err.Error(), nil, e.ArtifactSHA)
		a.emit(id, "error", "deploy", err.Error(), nil)
		return
	}
	a.setExperiment(id, "running", "", nodes, e.ArtifactSHA)
	a.emit(id, "info", "lifecycle", "experiment running", map[string]any{"nodes": len(nodes)})
	go a.monitor(id)
}

func (a *API) monitor(id string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		var exp model.Experiment
		servers := map[string]model.Server{}
		running := false
		a.store.View(func(s model.State) {
			for _, e := range s.Experiments {
				if e.ID == id {
					exp = e
					running = e.Status == "running"
				}
			}
			for _, x := range s.Servers {
				servers[x.ID] = x
			}
		})
		if !running {
			return
		}
		for _, node := range exp.Nodes {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = a.sampleNode(ctx, exp, node, servers[node.ServerID], "monitor")
			cancel()
		}
	}
}

func (a *API) sampleNode(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, reason string) error {
	expr := `(function(){var z=null,e="";try{z=eth.getAccountState()}catch(x){e=x.toString()}return JSON.stringify({block:eth.blockNumber.toString(),peers:net.peerCount.toString(),account:eth.accounts[0],publicBalance:eth.getBalance(eth.accounts[0]).toString(10),zk:z,zkError:e})})()`
	out, err := a.orch.Attach(ctx, exp, node, server, expr)
	if err != nil {
		a.setNodeError(exp.ID, node.ID, "unreachable", err.Error())
		return err
	}
	raw, err := orchestrator.ExtractJSONString(out)
	if err != nil {
		a.setNodeError(exp.ID, node.ID, "unreachable", err.Error())
		return err
	}
	var sample struct {
		Block         string `json:"block"`
		Peers         string `json:"peers"`
		Account       string `json:"account"`
		PublicBalance string `json:"publicBalance"`
		ZK            *struct {
			Balance     string `json:"balance"`
			Commitment  string `json:"commitment"`
			LastTxBlock string `json:"lastTxBlockNumber"`
		} `json:"zk"`
		ZKError string `json:"zkError"`
	}
	if err = json.Unmarshal([]byte(raw), &sample); err != nil {
		a.setNodeError(exp.ID, node.ID, "unreachable", err.Error())
		return err
	}
	base := 10
	blockText := sample.Block
	if strings.HasPrefix(blockText, "0x") {
		base = 16
		blockText = strings.TrimPrefix(blockText, "0x")
	}
	block, _ := strconv.ParseUint(blockText, base, 64)
	peers, _ := strconv.Atoi(sample.Peers)
	now := time.Now()
	return a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			if s.Experiments[i].ID != exp.ID {
				continue
			}
			for j := range s.Experiments[i].Nodes {
				n := &s.Experiments[i].Nodes[j]
				if n.ID != node.ID {
					continue
				}
				oldBalance, oldCommitment, firstSample := n.ZKBalance, n.Commitment, n.LastSeen.IsZero()
				n.Status = "running"
				n.Block = block
				n.Peers = peers
				n.Account = sample.Account
				n.PublicBalance = sample.PublicBalance
				n.StateError = sample.ZKError
				n.LastSeen = now
				if sample.ZK != nil {
					n.ZKBalance = sample.ZK.Balance
					n.Commitment = sample.ZK.Commitment
					n.LastTxBlock = sample.ZK.LastTxBlock
					if firstSample || reason != "monitor" || oldBalance != n.ZKBalance || oldCommitment != n.Commitment {
						s.AccountSnapshots = append(s.AccountSnapshots, model.AccountSnapshot{ID: id("snap"), ExperimentID: exp.ID, NodeID: node.ID, Account: n.Account, PublicBalance: n.PublicBalance, ZKBalance: n.ZKBalance, Commitment: n.Commitment, LastTxBlock: n.LastTxBlock, ChainBlock: block, Reason: reason, At: now})
					}
				}
			}
		}
		return nil
	})
}

func (a *API) setNodeError(experimentID, nodeID, status, message string) {
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			if s.Experiments[i].ID == experimentID {
				for j := range s.Experiments[i].Nodes {
					if s.Experiments[i].Nodes[j].ID == nodeID {
						s.Experiments[i].Nodes[j].Status = status
						s.Experiments[i].Nodes[j].StateError = message
					}
				}
			}
		}
		return nil
	})
}
func (a *API) stop(id string, e model.Experiment, servers map[string]model.Server) {
	a.setExperiment(id, "stopping", "", nil, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := a.orch.Stop(ctx, e, servers, func(l, k, m string, f map[string]any) { a.emit(id, l, k, m, f) }); err != nil {
		a.setExperiment(id, "failed", err.Error(), nil, "")
		return
	}
	a.setExperiment(id, "stopped", "", nil, "")
}

func (a *API) transactions(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.store.View(func(s model.State) { jsonOut(w, 200, s.Transactions) })
		return
	}
	if r.Method != "POST" {
		fail(w, 405, errors.New("method not allowed"))
		return
	}
	var t model.Transaction
	if err := decode(r, &t); err != nil {
		fail(w, 400, err)
		return
	}
	allowed := map[string]bool{"createAccount": true, "mint": true, "redeem": true, "public": true, "transfer": true}
	if !allowed[t.Type] {
		fail(w, 400, errors.New("unsupported transaction type"))
		return
	}
	t.ID = id("tx")
	t.Status = "queued"
	t.SubmittedAt = time.Now()
	if err := a.store.Update(func(s *model.State) error { s.Transactions = append(s.Transactions, t); return nil }); err != nil {
		fail(w, 500, err)
		return
	}
	go a.runTransaction(t)
	jsonOut(w, 202, t)
}

func (a *API) runTransaction(t model.Transaction) {
	lockIDs := []string{t.FromNode}
	if t.Type == "transfer" && t.ToNode != "" && t.ToNode != t.FromNode {
		lockIDs = append(lockIDs, t.ToNode)
		sort.Strings(lockIDs)
	}
	locks := make([]*sync.Mutex, 0, len(lockIDs))
	for _, lockID := range lockIDs {
		lockAny, _ := a.nodeLocks.LoadOrStore(lockID, &sync.Mutex{})
		lock := lockAny.(*sync.Mutex)
		lock.Lock()
		locks = append(locks, lock)
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	var exp model.Experiment
	var node model.Node
	var server model.Server
	var toNode model.Node
	ok := false
	a.store.View(func(s model.State) {
		for _, e := range s.Experiments {
			if e.ID == t.ExperimentID {
				exp = e
				for _, n := range e.Nodes {
					if n.ID == t.FromNode {
						node = n
					}
					if n.ID == t.ToNode {
						toNode = n
					}
				}
			}
		}
		for _, x := range s.Servers {
			if x.ID == node.ServerID {
				server = x
				ok = true
			}
		}
	})
	if !ok {
		a.finishTx(t.ID, "failed", "", errors.New("experiment, node or server not found"))
		return
	}
	if exp.Status != "running" {
		a.finishTx(t.ID, "failed", "", errors.New("experiment is not running"))
		return
	}
	value := t.Value
	if value == "" {
		value = "0x1"
	}
	expr := ""
	if t.Type == "transfer" {
		a.setTxCommand(t.ID, transferCommand(node.Name, toNode.Name, value))
		a.runTransfer(t, exp, node, toNode, server, value)
		return
	}
	switch t.Type {
	case "createAccount":
		expr = "eth.sendCreateAccountTransaction({from:eth.accounts[0]})"
	case "mint":
		expr = "eth.sendMintTransaction({from:eth.accounts[0],value:" + strconv.Quote(value) + "})"
	case "redeem":
		expr = "eth.sendRedeemTransaction({from:eth.accounts[0],value:" + strconv.Quote(value) + "})"
	case "public":
		if toNode.Account == "" {
			a.finishTx(t.ID, "failed", "", errors.New("destination node/account is required"))
			return
		}
		expr = "eth.sendPublicTransaction({from:eth.accounts[0],to:" + strconv.Quote(toNode.Account) + ",value:" + strconv.Quote(value) + "})"
	}
	a.setTxCommand(t.ID, node.Name+": "+expr)
	a.updateTx(t.ID, "proving", "", "")
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := a.orch.Attach(ctx, exp, node, server, expr)
	if err != nil {
		a.finishTx(t.ID, "failed", "", fmt.Errorf("%w: %s", err, out))
		return
	}
	hash := orchestrator.ParseHash(out)
	if hash == "" {
		message := strings.TrimSpace(out)
		if message == "" {
			message = "node returned no transaction hash"
		}
		a.finishTx(t.ID, "failed", "", fmt.Errorf("transaction was not submitted: %s", message))
		return
	}
	proofMs := time.Since(started).Milliseconds()
	proofUs := time.Since(started).Microseconds()
	parsedProofUs, parsedVerifyUs := orchestrator.ParseProofTimesMicros(out)
	if logProofUs, logVerifyUs, timingErr := a.orch.TransactionTimings(ctx, exp, node, server, hash); timingErr == nil {
		parsedProofUs, parsedVerifyUs = logProofUs, logVerifyUs
	}
	if parsedProofUs > 0 {
		proofUs = parsedProofUs
	}
	proofMs = proofUs / 1000
	a.updateTx(t.ID, "submitted", hash, "")
	a.emit(t.ExperimentID, "info", "transaction", "transaction submitted", map[string]any{"id": t.ID, "hash": hash, "proofDurationMs": proofMs})
	for i := 0; i < 300; i++ {
		out, err = a.orch.Attach(ctx, exp, node, server, "JSON.stringify(eth.getTransactionReceipt("+strconv.Quote(hash)+"))")
		if err == nil {
			if receipt, block, receiptStatus, ok := parseReceipt(out); ok {
				a.confirmTx(t.ID, hash, receipt, block, receiptStatus, proofUs, parsedVerifyUs)
				a.refreshTransactionNodes(exp, node, toNode, server, t.ID)
				if t.Type != "public" {
					a.waitForNextBlock(ctx, exp, node, server, block)
				}
				return
			}
		}
		time.Sleep(time.Second)
	}
	a.finishTx(t.ID, "timeout", hash, errors.New("receipt not observed within 5 minutes"))
}

func (a *API) runTransfer(t model.Transaction, exp model.Experiment, payer, receiver model.Node, payerServer model.Server, value string) {
	if receiver.ID == "" {
		a.finishTx(t.ID, "failed", "", errors.New("destination node is required for transfer"))
		return
	}
	if receiver.ID == payer.ID {
		a.finishTx(t.ID, "failed", "", errors.New("payer and receiver must be different nodes"))
		return
	}
	var receiverServer model.Server
	a.store.View(func(s model.State) {
		for _, x := range s.Servers {
			if x.ID == receiver.ServerID {
				receiverServer = x
			}
		}
	})
	if receiverServer.ID == "" {
		a.finishTx(t.ID, "failed", "", errors.New("destination server not found"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	// A transfer proof requires both current commitments to exist in the
	// global state Merkle tree. Query both nodes immediately so a missing
	// private account is reported before the expensive proof generation.
	_ = a.sampleNode(ctx, exp, payer, payerServer, "transfer-preflight:"+t.ID)
	_ = a.sampleNode(ctx, exp, receiver, receiverServer, "transfer-preflight:"+t.ID)
	a.store.View(func(s model.State) {
		for _, e := range s.Experiments {
			if e.ID != exp.ID {
				continue
			}
			for _, n := range e.Nodes {
				if n.ID == payer.ID {
					payer = n
				}
				if n.ID == receiver.ID {
					receiver = n
				}
			}
		}
	})
	if !privateStateOnChain(payer.LastTxBlock) {
		a.finishTx(t.ID, "failed", "", fmt.Errorf("payer %s has no private account state on chain; run CreateAccount and Mint first", payer.Name))
		return
	}
	if !privateStateOnChain(receiver.LastTxBlock) {
		a.finishTx(t.ID, "failed", "", fmt.Errorf("receiver %s has no private account state on chain; run CreateAccount first", receiver.Name))
		return
	}
	a.updateTx(t.ID, "proving", "", "")
	started := time.Now()
	out, err := a.orch.Attach(ctx, exp, payer, payerServer, "JSON.stringify(eth.getPayerNextState('0x01',"+strconv.Quote(value)+"))")
	if err != nil {
		a.finishTx(t.ID, "failed", "", fmt.Errorf("payer proof: %w: %s", err, out))
		return
	}
	raw, extractErr := orchestrator.ExtractJSONString(out)
	if extractErr != nil {
		_, _ = a.orch.Attach(ctx, exp, payer, payerServer, "eth.revertTransferState()")
		a.finishTx(t.ID, "failed", "", extractErr)
		return
	}
	var proof struct {
		CMT   string `json:"cmtANew"`
		SN    string `json:"snAOld"`
		Proof string `json:"proofA"`
	}
	if err := json.Unmarshal([]byte(raw), &proof); err != nil || proof.Proof == "" {
		_, _ = a.orch.Attach(ctx, exp, payer, payerServer, "eth.revertTransferState()")
		a.finishTx(t.ID, "failed", "", fmt.Errorf("decode payer proof: %w", err))
		return
	}
	expr := "eth.sendTransferTransaction({from:eth.accounts[0],value:" + strconv.Quote(value) + ",rs:'0x01',cmtANew:" + strconv.Quote(proof.CMT) + ",snAOld:" + strconv.Quote(proof.SN) + ",proofA:" + strconv.Quote(proof.Proof) + "})"
	payerProofUs, payerVerifyUs := orchestrator.ParseProofTimesMicros(out)
	out, err = a.orch.Attach(ctx, exp, receiver, receiverServer, expr)
	if err != nil {
		_, _ = a.orch.Attach(ctx, exp, payer, payerServer, "eth.revertTransferState()")
		a.finishTx(t.ID, "failed", "", fmt.Errorf("receiver submit: %w: %s", err, out))
		return
	}
	hash := orchestrator.ParseHash(out)
	if hash == "" {
		_, _ = a.orch.Attach(ctx, exp, payer, payerServer, "eth.revertTransferState()")
		message := strings.TrimSpace(out)
		if message == "" {
			message = "node returned no transaction hash"
		}
		a.finishTx(t.ID, "failed", "", fmt.Errorf("receiver transaction was not submitted: %s", message))
		return
	}
	receiverProofUs, receiverVerifyUs := orchestrator.ParseProofTimesMicros(out)
	if _, logVerifyUs, timingErr := a.orch.TransactionTimings(ctx, exp, receiver, receiverServer, hash); timingErr == nil {
		receiverVerifyUs = logVerifyUs
	}
	proofUs := payerProofUs + receiverProofUs
	if proofUs == 0 {
		proofUs = time.Since(started).Microseconds()
	}
	verifyUs := payerVerifyUs + receiverVerifyUs
	a.updateTx(t.ID, "submitted", hash, "")
	for i := 0; i < 600; i++ {
		out, err = a.orch.Attach(ctx, exp, receiver, receiverServer, "JSON.stringify(eth.getTransactionReceipt("+strconv.Quote(hash)+"))")
		if err == nil {
			if receipt, block, receiptStatus, ok := parseReceipt(out); ok {
				a.confirmTx(t.ID, hash, receipt, block, receiptStatus, proofUs, verifyUs)
				a.refreshTransactionNodes(exp, payer, receiver, payerServer, t.ID)
				a.waitForNextBlock(ctx, exp, receiver, receiverServer, block)
				return
			}
		}
		time.Sleep(time.Second)
	}
	a.finishTx(t.ID, "timeout", hash, errors.New("transfer receipt not observed"))
}

func privateStateOnChain(block string) bool {
	block = strings.TrimSpace(strings.ToLower(block))
	return block != "" && block != "0" && block != "0x0"
}

func transferCommand(fromName, toName, value string) string {
	return fromName + ": JSON.stringify(eth.getPayerNextState('0x01'," + strconv.Quote(value) + "))\n" +
		toName + ": eth.sendTransferTransaction({from:eth.accounts[0],value:" + strconv.Quote(value) + ",rs:'0x01',cmtANew:<payer.cmtANew>,snAOld:<payer.snAOld>,proofA:<payer.proofA>})"
}

func parseBlockValue(value string) (uint64, bool) {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	base := 10
	if strings.HasPrefix(strings.ToLower(value), "0x") {
		base = 16
		value = value[2:]
	}
	n, err := strconv.ParseUint(value, base, 64)
	return n, err == nil
}

func (a *API) waitForNextBlock(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, receiptBlock string) {
	target, ok := parseBlockValue(receiptBlock)
	if !ok {
		return
	}
	for i := 0; i < 120; i++ {
		out, err := a.orch.Attach(ctx, exp, node, server, "eth.blockNumber.toString()")
		if err == nil {
			if current, valid := parseBlockValue(out); valid && current > target {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func parseReceipt(out string) (raw, block, status string, ok bool) {
	raw, err := orchestrator.ExtractJSONString(out)
	if err != nil || raw == "" || raw == "null" {
		return "", "", "", false
	}
	var receipt struct {
		BlockNumber any `json:"blockNumber"`
		Status      any `json:"status"`
	}
	if json.Unmarshal([]byte(raw), &receipt) != nil {
		return "", "", "", false
	}
	if receipt.BlockNumber != nil {
		block = fmt.Sprint(receipt.BlockNumber)
	}
	if receipt.Status != nil {
		status = fmt.Sprint(receipt.Status)
	}
	return raw, block, status, true
}

func (a *API) confirmTx(id, hash, receipt, block, receiptStatus string, proofUs, verifyUs int64) {
	finalStatus := "confirmed"
	var txErr error
	if receiptStatus == "0" || receiptStatus == "0x0" || receiptStatus == "false" {
		finalStatus = "failed"
		txErr = errors.New("transaction reverted on chain")
	}
	a.finishTx(id, finalStatus, hash, txErr)
	_ = a.store.Update(func(s *model.State) error {
		for j := range s.Transactions {
			if s.Transactions[j].ID == id {
				s.Transactions[j].ProofDurationUs = proofUs
				s.Transactions[j].VerifyDurationUs = verifyUs
				s.Transactions[j].ProofDurationMs = proofUs / 1000
				s.Transactions[j].VerifyDurationMs = verifyUs / 1000
				s.Transactions[j].BlockNumber = block
				s.Transactions[j].ReceiptStatus = receiptStatus
				s.Transactions[j].Receipt = receipt
			}
		}
		return nil
	})
}

func (a *API) refreshTransactionNodes(exp model.Experiment, from, to model.Node, fromServer model.Server, txID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = a.sampleNode(ctx, exp, from, fromServer, "transaction:"+txID)
	if to.ID == "" || to.ID == from.ID {
		return
	}
	var toServer model.Server
	a.store.View(func(s model.State) {
		for _, candidate := range s.Servers {
			if candidate.ID == to.ServerID {
				toServer = candidate
			}
		}
	})
	if toServer.ID != "" {
		_ = a.sampleNode(ctx, exp, to, toServer, "transaction:"+txID)
	}
}

func (a *API) workloads(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.store.View(func(s model.State) { jsonOut(w, 200, s.Workloads) })
		return
	}
	if r.Method != "POST" {
		fail(w, 405, errors.New("method not allowed"))
		return
	}
	var v model.Workload
	if err := decode(r, &v); err != nil {
		fail(w, 400, err)
		return
	}
	if v.ExperimentID == "" || v.Type == "" || v.RatePerSecond <= 0 || v.DurationSeconds < 1 {
		fail(w, 400, errors.New("experimentId, type, positive ratePerSecond and durationSeconds are required"))
		return
	}
	if !map[string]bool{"createAccount": true, "mint": true, "transfer": true, "redeem": true, "public": true}[v.Type] {
		fail(w, 400, errors.New("unsupported workload transaction type"))
		return
	}
	if v.Strategy == "" {
		v.Strategy = "round-robin"
	}
	if v.Value == "" {
		v.Value = "0x1"
	}
	if v.Name == "" {
		v.Name = v.Type + " workload"
	}
	v.ID = id("load")
	v.Status = "queued"
	v.CreatedAt = time.Now()
	if err := a.store.Update(func(s *model.State) error { s.Workloads = append(s.Workloads, v); return nil }); err != nil {
		fail(w, 500, err)
		return
	}
	go a.runWorkload(v)
	jsonOut(w, 202, v)
}

func (a *API) runWorkload(v model.Workload) {
	var exp model.Experiment
	found := false
	a.store.View(func(s model.State) {
		for _, e := range s.Experiments {
			if e.ID == v.ExperimentID {
				exp = e
				found = true
			}
		}
	})
	if !found || exp.Status != "running" || len(exp.Nodes) == 0 {
		a.finishWorkload(v.ID, "failed", "experiment is not running or has no nodes")
		return
	}
	a.updateWorkload(v.ID, "running", 0)
	interval := time.Duration(float64(time.Second) / v.RatePerSecond)
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.NewTimer(time.Duration(v.DurationSeconds) * time.Second)
	defer deadline.Stop()
	submitted := 0
	for {
		select {
		case <-deadline.C:
			a.updateWorkload(v.ID, "draining", submitted)
			a.emit(v.ExperimentID, "info", "workload", "workload stopped submitting; waiting for transactions", map[string]any{"id": v.ID, "submitted": submitted})
			a.drainWorkload(v, submitted)
			return
		case <-ticker.C:
			node := exp.Nodes[submitted%len(exp.Nodes)]
			toNode := ""
			if v.Type == "transfer" || v.Type == "public" {
				if len(exp.Nodes) < 2 {
					a.finishWorkload(v.ID, "failed", "transfer/public workload requires at least two nodes")
					return
				}
				toNode = exp.Nodes[(submitted+1)%len(exp.Nodes)].ID
			}
			tx := model.Transaction{ID: id("tx"), WorkloadID: v.ID, Sequence: submitted + 1, ExperimentID: v.ExperimentID, Type: v.Type, FromNode: node.ID, ToNode: toNode, Value: v.Value, Status: "queued", SubmittedAt: time.Now()}
			if err := a.store.Update(func(s *model.State) error {
				s.Transactions = append(s.Transactions, tx)
				for i := range s.Workloads {
					if s.Workloads[i].ID == v.ID {
						s.Workloads[i].Submitted++
					}
				}
				return nil
			}); err != nil {
				a.finishWorkload(v.ID, "failed", err.Error())
				return
			}
			submitted++
			go a.runTransaction(tx)
		}
	}
}

func (a *API) drainWorkload(v model.Workload, submitted int) {
	deadline := time.NewTimer(30 * time.Minute)
	ticker := time.NewTicker(time.Second)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			a.finishWorkload(v.ID, "timeout", "transactions did not drain within 30 minutes")
			return
		case <-ticker.C:
			terminal, confirmed, failed := 0, 0, 0
			a.store.View(func(s model.State) {
				for _, tx := range s.Transactions {
					if tx.WorkloadID != v.ID {
						continue
					}
					switch tx.Status {
					case "confirmed":
						confirmed++
						terminal++
					case "failed", "timeout":
						failed++
						terminal++
					}
				}
			})
			if terminal < submitted {
				continue
			}
			status := "completed"
			errText := ""
			if failed > 0 {
				status = "completed-with-errors"
				errText = fmt.Sprintf("%d of %d transactions failed", failed, submitted)
			}
			a.finishWorkload(v.ID, status, errText)
			a.emit(v.ExperimentID, "info", "workload", "workload transactions drained", map[string]any{"id": v.ID, "submitted": submitted, "confirmed": confirmed, "failed": failed})
			return
		}
	}
}

func (a *API) updateWorkload(id, status string, submitted int) {
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Workloads {
			if s.Workloads[i].ID == id {
				s.Workloads[i].Status = status
				if submitted > 0 {
					s.Workloads[i].Submitted = submitted
				}
				if status == "running" {
					s.Workloads[i].StartedAt = time.Now()
				}
			}
		}
		return nil
	})
}
func (a *API) finishWorkload(id, status, errText string) {
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Workloads {
			if s.Workloads[i].ID == id {
				s.Workloads[i].Status = status
				s.Workloads[i].Error = errText
				s.Workloads[i].FinishedAt = time.Now()
			}
		}
		return nil
	})
}

func percentile(v []int64, p float64) int64 {
	if len(v) == 0 {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	i := int(float64(len(v)-1) * p)
	return v[i]
}
func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	eid := r.URL.Query().Get("experimentId")
	var txs []model.Transaction
	a.store.View(func(s model.State) {
		for _, t := range s.Transactions {
			if eid == "" || t.ExperimentID == eid {
				txs = append(txs, t)
			}
		}
	})
	var confirmed, failed int
	var latencies, proofsUs, verifiesUs, chainConfirmUs []int64
	var first, last time.Time
	for _, t := range txs {
		if first.IsZero() || t.SubmittedAt.Before(first) {
			first = t.SubmittedAt
		}
		if t.Status == "confirmed" {
			confirmed++
			d := t.ConfirmedAt.Sub(t.SubmittedAt).Milliseconds()
			latencies = append(latencies, d)
			proofUs := t.ProofDurationUs
			if proofUs == 0 {
				proofUs = t.ProofDurationMs * 1000
			}
			verifyUs := t.VerifyDurationUs
			if verifyUs == 0 {
				verifyUs = t.VerifyDurationMs * 1000
			}
			if proofUs > 0 {
				proofsUs = append(proofsUs, proofUs)
			}
			if verifyUs > 0 {
				verifiesUs = append(verifiesUs, verifyUs)
			}
			if !t.BroadcastAt.IsZero() {
				chainConfirmUs = append(chainConfirmUs, t.ConfirmedAt.Sub(t.BroadcastAt).Microseconds())
			}
			if last.IsZero() || t.ConfirmedAt.After(last) {
				last = t.ConfirmedAt
			}
		} else if t.Status == "failed" || t.Status == "timeout" {
			failed++
		}
	}
	duration := last.Sub(first).Seconds()
	tps := 0.0
	if duration > 0 {
		tps = float64(confirmed) / duration
	}
	jsonOut(w, 200, map[string]any{"total": len(txs), "confirmed": confirmed, "failed": failed, "successRate": func() float64 {
		if confirmed+failed == 0 {
			return 0
		}
		return float64(confirmed) / float64(confirmed+failed)
	}(), "confirmedTPS": tps, "latencyP50Ms": percentile(latencies, .50), "latencyP95Ms": percentile(latencies, .95), "proofP50Us": percentile(proofsUs, .50), "proofP95Us": percentile(proofsUs, .95), "verifyP50Us": percentile(verifiesUs, .50), "verifyP95Us": percentile(verifiesUs, .95), "chainConfirmP50Us": percentile(chainConfirmUs, .50), "chainConfirmP95Us": percentile(chainConfirmUs, .95)})
}
func (a *API) updateTx(id, status, hash, errText string) {
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Transactions {
			if s.Transactions[i].ID == id {
				s.Transactions[i].Status = status
				if status == "proving" && s.Transactions[i].ProvingAt.IsZero() {
					s.Transactions[i].ProvingAt = time.Now()
				}
				if status == "submitted" && s.Transactions[i].BroadcastAt.IsZero() {
					s.Transactions[i].BroadcastAt = time.Now()
				}
				if hash != "" {
					s.Transactions[i].Hash = hash
				}
				s.Transactions[i].Error = errText
			}
		}
		return nil
	})
}

func (a *API) setTxCommand(id, command string) {
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Transactions {
			if s.Transactions[i].ID == id {
				s.Transactions[i].Command = command
			}
		}
		return nil
	})
}
func (a *API) finishTx(id, status, hash string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Transactions {
			if s.Transactions[i].ID == id {
				s.Transactions[i].Status = status
				s.Transactions[i].Hash = hash
				s.Transactions[i].Error = msg
				s.Transactions[i].ConfirmedAt = time.Now()
			}
		}
		return nil
	})
}
func (a *API) transactionAction(w http.ResponseWriter, r *http.Request) {
	fail(w, 501, errors.New("transaction action not implemented"))
}

func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan model.Event, 64)
	a.mu.Lock()
	a.subscribers[ch] = struct{}{}
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.subscribers, ch); a.mu.Unlock() }()
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	f.Flush()
	for {
		select {
		case e := <-ch:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
			f.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
