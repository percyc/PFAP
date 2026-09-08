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
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/orchestrator"
	"github.com/pfap/lab/internal/store"
)

type API struct {
	store              *store.Store
	orch               orchestrator.Orchestrator
	subscribers        map[chan model.Event]struct{}
	mu                 sync.Mutex
	nodeLocks          sync.Map
	serverSetupLocks   sync.Map
	lifecycleMu        sync.Mutex
	stopWaiters        atomic.Int32
	knownHostsMu       sync.Mutex
	reconcileLocks     sync.Map
	pendingUncertainty sync.Map
	pendingTxFinishes  sync.Map
	workloadDrainLocks sync.Map
	diskPlans          sync.Map
	serverMaintenance  sync.Map
	startupError       error
}

func New(s *store.Store) *API {
	a := &API{store: s, subscribers: map[chan model.Event]struct{}{}}
	var running []string
	err := s.Update(func(state *model.State) error {
		migrateMiningState(state)
		reconcileInterruptedTasks(state)
		for i := range state.Experiments {
			for j := range state.Experiments[i].Nodes {
				n := &state.Experiments[i].Nodes[j]
				if n.Status == "recovering" {
					n.Status = "unreachable"
					n.RecoveryError = "控制器重启中断了恢复检查，请重新点击恢复节点；不会重新发送交易。"
				}
			}
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
	if err != nil {
		a.startupError = fmt.Errorf("控制器初始化状态保存失败，修复存储后请重启 Lab：%w", err)
		return a
	}
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
	go a.monitorServers()
	go a.monitorUncertainTransactions()
	go a.monitorLogRotation()
	return a
}

func (a *API) monitorServers() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		var servers []model.Server
		a.store.View(func(s model.State) { servers = s.Servers })
		for _, server := range servers {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			out, err := a.orch.Check(ctx, server)
			cancel()
			_ = a.recordServerCheck(server.ID, out, err)
		}
		<-ticker.C
	}
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
		if r.URL.Path == "/api/storage-health" && r.Method == http.MethodGet {
			jsonOut(w, http.StatusOK, a.store.Health())
			return
		}
		if a.startupError != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			fail(w, http.StatusServiceUnavailable, a.startupError)
			return
		}
		if r.URL.Path == "/api/build-profile" && r.Method == http.MethodGet {
			b, err := os.ReadFile("dist/build-profile.json")
			if errors.Is(err, os.ErrNotExist) {
				jsonOut(w, 200, map[string]any{})
				return
			}
			if err != nil {
				fail(w, 500, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
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
		if r.URL.Path == "/api/servers/batch/trust-host-keys" {
			a.batchTrustServerHostKeys(w, r)
			return
		}
		if r.URL.Path == "/api/servers/batch/delete" {
			a.batchDeleteServers(w, r)
			return
		}
		if r.URL.Path == "/api/servers/batch/host-group" {
			a.batchServerHostGroup(w, r)
			return
		}
		if r.URL.Path == "/api/servers/batch" {
			a.batchServers(w, r)
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
		if strings.HasPrefix(r.URL.Path, "/api/workloads/") {
			a.workloadAction(w, r)
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
	if exp.MiningStatus == "updating" {
		fail(w, http.StatusConflict, errors.New("矿工配置正在应用，请等待完成后再发送手动指令"))
		return
	}

	lockAny, _ := a.nodeLocks.LoadOrStore(node.ID, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	flowBusy := false
	a.store.View(func(s model.State) {
		for _, run := range s.Workloads {
			if run.ExperimentID == exp.ID && run.Strategy == "ready-pool" && runActive(run) {
				flowBusy = true
			}
		}
	})
	if flowBusy {
		fail(w, http.StatusConflict, errors.New("完整实验流程运行中，节点控制台暂不可执行命令；请先停止并收尾"))
		return
	}
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
	if err := a.normalizeServer(&v); err != nil {
		fail(w, 400, err)
		return
	}
	v.ID = id("srv")
	v.Status = "unknown"
	v.LastCheck, v.LastSuccessAt = time.Time{}, time.Time{}
	v.SystemInfo, v.LastError = "", ""
	v.CreatedAt = time.Now()
	if err := a.store.Update(func(s *model.State) error {
		if duplicateServer(s.Servers, v, "") {
			return errors.New("a server with the same host and port already exists")
		}
		s.Servers = append(s.Servers, v)
		return nil
	}); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			fail(w, http.StatusConflict, err)
			return
		}
		fail(w, 500, err)
		return
	}
	jsonOut(w, 201, v)
}

func (a *API) normalizeServer(v *model.Server) error {
	v.Name, v.Host, v.P2PHost, v.User = strings.TrimSpace(v.Name), strings.TrimSpace(v.Host), strings.TrimSpace(v.P2PHost), strings.TrimSpace(v.User)
	v.IdentityFile, v.WorkDir = strings.TrimSpace(v.IdentityFile), strings.TrimSpace(v.WorkDir)
	var groupErr error
	v.HostGroup, groupErr = normalizeHostGroup(v.HostGroup)
	if groupErr != nil {
		return groupErr
	}
	if v.Name == "" || v.Host == "" || v.User == "" {
		return errors.New("name, host and user are required")
	}
	if v.Port == 0 {
		v.Port = 22
	}
	if v.Port < 1 || v.Port > 65535 {
		return errors.New("SSH port must be between 1 and 65535")
	}
	if v.WorkDir == "" {
		v.WorkDir = "/opt/pfap-lab"
	}
	if !filepath.IsAbs(v.WorkDir) {
		return errors.New("work directory must be an absolute path")
	}
	if v.Host != "local" && v.Host != "localhost-local" && v.KnownHostsFile == "" {
		v.KnownHostsFile = filepath.Join(a.store.DataDir(), "known_hosts")
	}
	return nil
}

func duplicateServer(servers []model.Server, candidate model.Server, excludeID string) bool {
	for _, existing := range servers {
		if existing.ID != excludeID && strings.EqualFold(existing.Host, candidate.Host) && existing.Port == candidate.Port {
			return true
		}
	}
	return false
}

func experimentUsesServer(e model.Experiment, serverID string) bool {
	for _, placement := range e.Placements {
		if placement.ServerID == serverID {
			return true
		}
	}
	return false
}

func activeExperimentStatus(status string) bool {
	return status == "running" || status == "deploying" || status == "resuming" || status == "stopping" || status == "stop-failed" || status == "interrupted"
}

type serverIDsRequest struct {
	IDs []string `json:"ids"`
}

func (a *API) requestedServers(r *http.Request) ([]model.Server, error) {
	var request serverIDsRequest
	if err := decode(r, &request); err != nil {
		return nil, err
	}
	if len(request.IDs) == 0 || len(request.IDs) > 100 {
		return nil, errors.New("batch must contain between 1 and 100 server IDs")
	}
	seen := make(map[string]bool, len(request.IDs))
	serversByID := map[string]model.Server{}
	a.store.View(func(s model.State) {
		for _, server := range s.Servers {
			serversByID[server.ID] = server
		}
	})
	servers := make([]model.Server, 0, len(request.IDs))
	for _, rawID := range request.IDs {
		serverID := strings.TrimSpace(rawID)
		if serverID == "" || seen[serverID] {
			return nil, fmt.Errorf("server ID %q is empty or duplicated", serverID)
		}
		seen[serverID] = true
		server, ok := serversByID[serverID]
		if !ok {
			return nil, fmt.Errorf("server %s not found", serverID)
		}
		servers = append(servers, server)
	}
	return servers, nil
}

func (a *API) appendKnownHostKeys(path, keys string) error {
	// Single-host and batch trust share this read-modify-rename lock, so one
	// successful trust operation cannot overwrite another operation's keys.
	a.knownHostsMu.Lock()
	defer a.knownHostsMu.Unlock()
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeKnownHostsAtomically(path, func(temp *os.File) error {
		_, err := temp.Write(append(existing, []byte(keys)...))
		return err
	})
}

func writeKnownHostsAtomically(path string, write func(*os.File) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".known-hosts-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err = temp.Chmod(0o600); err == nil {
		err = write(temp)
	}
	closeErr := temp.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return os.Rename(tempName, path)
}

func (a *API) batchTrustServerHostKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	servers, err := a.requestedServers(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	keysByPath := map[string]string{}
	pathsByID := map[string]string{}
	// Scan every selected host before changing known_hosts. A failed scan leaves
	// the trust store untouched, so the operator can safely retry the whole set.
	for _, server := range servers {
		if server.Host == "local" || server.Host == "localhost-local" {
			fail(w, http.StatusBadRequest, fmt.Errorf("server %s is local and does not use SSH host keys", server.Name))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		keys, scanErr := a.orch.Remote.ScanHostKey(ctx, server)
		cancel()
		if scanErr != nil {
			fail(w, http.StatusBadGateway, fmt.Errorf("scan %s (%s): %w", server.Name, server.Host, scanErr))
			return
		}
		path := server.KnownHostsFile
		if path == "" {
			path = filepath.Join(a.store.DataDir(), "known_hosts")
		}
		pathsByID[server.ID] = path
		keysByPath[path] += keys
	}
	for path, keys := range keysByPath {
		if err := a.appendKnownHostKeys(path, keys); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := a.store.Update(func(s *model.State) error {
		for i := range s.Servers {
			if path := pathsByID[s.Servers[i].ID]; path != "" {
				s.Servers[i].KnownHostsFile = path
			}
		}
		return nil
	}); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"status": "trusted", "trusted": len(servers)})
}

func (a *API) batchDeleteServers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	servers, err := a.requestedServers(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	selected := make(map[string]bool, len(servers))
	for _, server := range servers {
		selected[server.ID] = true
	}
	err = a.store.Update(func(s *model.State) error {
		for serverID := range selected {
			if _, busy := a.serverMaintenance.Load(serverID); busy {
				return errors.New("服务器正在执行磁盘维护，请稍后删除")
			}
		}
		for _, e := range s.Experiments {
			if !activeExperimentStatus(e.Status) && e.Status != "draft" {
				continue
			}
			for serverID := range selected {
				if experimentUsesServer(e, serverID) {
					return fmt.Errorf("server %s is used by %s experiment %s", serverID, e.Status, e.Name)
				}
			}
		}
		kept := s.Servers[:0]
		for _, server := range s.Servers {
			if !selected[server.ID] {
				kept = append(kept, server)
			}
		}
		s.Servers = kept
		return nil
	})
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"status": "deleted", "deleted": len(servers)})
}

func (a *API) batchServers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var request struct {
		Hosts        []string `json:"hosts"`
		NamePrefix   string   `json:"namePrefix"`
		User         string   `json:"user"`
		Port         int      `json:"port"`
		IdentityFile string   `json:"identityFile"`
		WorkDir      string   `json:"workDir"`
		HostGroup    string   `json:"hostGroup"`
	}
	if err := decode(r, &request); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if len(request.Hosts) == 0 || len(request.Hosts) > 100 {
		fail(w, http.StatusBadRequest, errors.New("batch must contain between 1 and 100 hosts"))
		return
	}
	prefix := strings.TrimSpace(request.NamePrefix)
	if prefix == "" {
		prefix = "worker"
	}
	seen := map[string]bool{}
	created := make([]model.Server, 0, len(request.Hosts))
	for i, host := range request.Hosts {
		host = strings.TrimSpace(host)
		key := strings.ToLower(host)
		if host == "" || seen[key] {
			fail(w, http.StatusBadRequest, fmt.Errorf("host %q is empty or duplicated", host))
			return
		}
		seen[key] = true
		candidate := model.Server{Name: fmt.Sprintf("%s-%02d", prefix, i+1), Host: host, P2PHost: host, Port: request.Port, User: request.User, IdentityFile: request.IdentityFile, WorkDir: request.WorkDir, HostGroup: request.HostGroup, Status: "unknown", CreatedAt: time.Now()}
		if err := a.normalizeServer(&candidate); err != nil {
			fail(w, http.StatusBadRequest, fmt.Errorf("%s: %w", host, err))
			return
		}
		candidate.ID = id("srv")
		created = append(created, candidate)
	}
	if err := a.store.Update(func(s *model.State) error {
		for _, candidate := range created {
			if duplicateServer(s.Servers, candidate, "") {
				return fmt.Errorf("%s:%d already exists", candidate.Host, candidate.Port)
			}
		}
		s.Servers = append(s.Servers, created...)
		return nil
	}); err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	jsonOut(w, http.StatusCreated, created)
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
	if len(parts) >= 4 && parts[3] == "disk" {
		a.serverDiskAction(w, r, server, parts)
		return
	}
	if len(parts) == 4 && parts[3] == "network" {
		a.serverNetwork(w, r, server)
		return
	}
	if len(parts) == 3 && r.Method == http.MethodPut {
		var request struct {
			model.Server
			HostGroup *string `json:"hostGroup"`
		}
		if err := decode(r, &request); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		updated := request.Server
		updated.HostGroup = server.HostGroup
		if request.HostGroup != nil {
			updated.HostGroup = *request.HostGroup
		}
		updated.ID, updated.CreatedAt = server.ID, server.CreatedAt
		updated.KnownHostsFile = server.KnownHostsFile
		updated.Labels = server.Labels
		if err := a.normalizeServer(&updated); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		criticalChanged := updated.Host != server.Host || updated.Port != server.Port || updated.User != server.User || updated.IdentityFile != server.IdentityFile || updated.WorkDir != server.WorkDir || updated.P2PHost != server.P2PHost
		if criticalChanged {
			updated.Status = "unknown"
			updated.LastCheck, updated.LastSuccessAt = time.Time{}, time.Time{}
			updated.SystemInfo, updated.LastError = "", ""
		} else {
			updated.Status, updated.LastCheck, updated.SystemInfo = server.Status, server.LastCheck, server.SystemInfo
			updated.LastError, updated.LastSuccessAt = server.LastError, server.LastSuccessAt
		}
		err := a.store.Update(func(s *model.State) error {
			if _, busy := a.serverMaintenance.Load(sid); busy {
				return errors.New("服务器正在执行磁盘维护，请稍后编辑")
			}
			if duplicateServer(s.Servers, updated, sid) {
				return errors.New("a server with the same host and port already exists")
			}
			if criticalChanged {
				for _, e := range s.Experiments {
					if activeExperimentStatus(e.Status) && experimentUsesServer(e, sid) {
						return fmt.Errorf("server is used by active experiment %s; stop it before changing connection settings", e.Name)
					}
				}
			}
			for i := range s.Servers {
				if s.Servers[i].ID == sid {
					if request.HostGroup == nil {
						updated.HostGroup = s.Servers[i].HostGroup
					}
					if !criticalChanged {
						current := s.Servers[i]
						updated.Status, updated.LastCheck, updated.LastSuccessAt = current.Status, current.LastCheck, current.LastSuccessAt
						updated.SystemInfo, updated.LastError = current.SystemInfo, current.LastError
					}
					s.Servers[i] = updated
					return nil
				}
			}
			return os.ErrNotExist
		})
		if err != nil {
			fail(w, http.StatusConflict, err)
			return
		}
		jsonOut(w, http.StatusOK, updated)
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
		if saveErr := a.recordServerCheck(sid, out, err); saveErr != nil {
			fail(w, http.StatusInternalServerError, saveErr)
			return
		}
		if err != nil {
			fail(w, 502, err)
			return
		}
		jsonOut(w, 200, map[string]string{"status": status, "info": out})
		return
	}
	if len(parts) == 4 && parts[3] == "trust-host-key" && r.Method == http.MethodPost {
		if server.Host == "local" || server.Host == "localhost-local" {
			fail(w, http.StatusBadRequest, errors.New("local server does not use SSH host keys"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		keys, err := a.orch.Remote.ScanHostKey(ctx, server)
		if err != nil {
			fail(w, http.StatusBadGateway, err)
			return
		}
		path := server.KnownHostsFile
		if path == "" {
			path = filepath.Join(a.store.DataDir(), "known_hosts")
		}
		if err := a.appendKnownHostKeys(path, keys); err != nil {
			fail(w, 500, err)
			return
		}
		_ = a.store.Update(func(s *model.State) error {
			for i := range s.Servers {
				if s.Servers[i].ID == sid {
					s.Servers[i].KnownHostsFile = path
				}
			}
			return nil
		})
		jsonOut(w, http.StatusOK, map[string]string{"status": "trusted", "knownHostsFile": path})
		return
	}
	if len(parts) == 3 && r.Method == "DELETE" {
		err := a.store.Update(func(s *model.State) error {
			if _, busy := a.serverMaintenance.Load(sid); busy {
				return errors.New("服务器正在执行磁盘维护，请稍后删除")
			}
			for _, e := range s.Experiments {
				if experimentUsesServer(e, sid) && (activeExperimentStatus(e.Status) || e.Status == "draft") {
					return fmt.Errorf("server is used by %s experiment %s", e.Status, e.Name)
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
	var request struct {
		model.Experiment
		MinerCount *int `json:"minerCount"`
	}
	if err := decode(r, &request); err != nil {
		fail(w, 400, err)
		return
	}
	e := request.Experiment
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
			if activeExperimentStatus(x.Status) {
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
	if totalNodes > 300 {
		fail(w, 400, errors.New("an experiment is limited to 300 nodes"))
		return
	}
	e.MinerCount = min(2, totalNodes)
	if request.MinerCount != nil {
		e.MinerCount = *request.MinerCount
	}
	if e.MinerMode != "manual" && (e.MinerCount < 1 || e.MinerCount > totalNodes) {
		fail(w, http.StatusBadRequest, fmt.Errorf("矿工数量必须在 1 到 %d 之间", totalNodes))
		return
	}
	if err := model.NormalizeMinerConfiguration(&e); err != nil {
		fail(w, http.StatusBadRequest, err)
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
	e.Nodes = nil
	e.ArtifactSHA, e.RecoveryArtifactSHA, e.Error = "", "", ""
	e.StartedAt, e.FinishedAt = time.Time{}, time.Time{}
	e.MiningStatus, e.MiningError = "", ""
	e.MiningUpdatedAt = time.Time{}
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
	if len(parts) == 6 && parts[3] == "nodes" && parts[5] == "recover" {
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return
		}
		a.recoverNode(w, parts[2], parts[4])
		return
	}
	if len(parts) == 3 && r.Method == http.MethodDelete {
		a.deleteExperiment(w, parts[2])
		return
	}
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
		a.exportResults(w, r, eid, "")
		return
	}
	if r.Method != "POST" {
		fail(w, 405, errors.New("method not allowed"))
		return
	}
	if action == "miners" {
		a.updateMiners(w, r, eid)
		return
	}
	if action == "initialize-accounts" {
		a.initializeAccounts(w, eid)
		return
	}
	a.lifecycleAction(w, eid, action)
}

func (a *API) initializeAccounts(w http.ResponseWriter, experimentID string) {
	batchID := id("init")
	queued := []model.Transaction{}
	alreadyInitialized, busy, unavailable, total := 0, 0, 0, 0
	err := a.store.Update(func(s *model.State) error {
		for _, run := range s.Workloads {
			if run.ExperimentID == experimentID && run.Strategy == "ready-pool" && runActive(run) {
				return errors.New("完整实验流程运行中，不能重新初始化账户")
			}
		}
		var experiment *model.Experiment
		for i := range s.Experiments {
			if s.Experiments[i].ID == experimentID {
				experiment = &s.Experiments[i]
				break
			}
		}
		if experiment == nil {
			return os.ErrNotExist
		}
		if experiment.Status != "running" {
			return errors.New("experiment must be running before accounts can be initialized")
		}
		total = len(experiment.Nodes)
		if total == 0 {
			return errors.New("experiment has no nodes")
		}
		for _, node := range experiment.Nodes {
			if privateAccountInitialized(s.Transactions, experimentID, node) {
				alreadyInitialized++
				continue
			}
			if node.Status != "running" || node.PrivateStateError != "" {
				unavailable++
				continue
			}
			if transactionNodesBusy(s.Transactions, node.ID, "") {
				busy++
				continue
			}
			tx := model.Transaction{
				ID:           id("tx"),
				BatchID:      batchID,
				ExperimentID: experimentID,
				Type:         "createAccount",
				FromNode:     node.ID,
				Status:       "queued",
				SubmittedAt:  time.Now(),
			}
			s.Transactions = append(s.Transactions, tx)
			queued = append(queued, tx)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		fail(w, http.StatusNotFound, errors.New("experiment not found"))
		return
	}
	if err != nil {
		for _, tx := range queued {
			_ = a.finishTx(tx.ID, "failed", "", fmt.Errorf("初始化入队保存未完成，未执行 RPC：%w", err))
		}
		fail(w, http.StatusConflict, err)
		return
	}
	for _, tx := range queued {
		go a.runTransaction(tx)
	}
	a.emit(experimentID, "info", "initialization", "batch CreateAccount queued", map[string]any{
		"batchId": batchID, "queued": len(queued), "alreadyInitialized": alreadyInitialized, "busy": busy, "unavailable": unavailable,
	})
	status := http.StatusAccepted
	if len(queued) == 0 {
		status = http.StatusOK
	}
	transactionIDs := make([]string, 0, len(queued))
	for _, tx := range queued {
		transactionIDs = append(transactionIDs, tx.ID)
	}
	jsonOut(w, status, map[string]any{
		"batchId": batchID, "total": total, "queued": len(queued), "alreadyInitialized": alreadyInitialized,
		"busy": busy, "unavailable": unavailable, "transactionIds": transactionIDs,
	})
}

func (a *API) deleteExperiment(w http.ResponseWriter, experimentID string) {
	if !a.lifecycleMu.TryLock() {
		fail(w, http.StatusConflict, errors.New("正在执行实验操作或磁盘维护，请稍后删除"))
		return
	}
	defer a.lifecycleMu.Unlock()
	err := a.store.Update(func(s *model.State) error {
		index := -1
		for i := range s.Experiments {
			if s.Experiments[i].ID == experimentID {
				index = i
				break
			}
		}
		if index < 0 {
			return os.ErrNotExist
		}
		exp := s.Experiments[index]
		if activeExperimentStatus(exp.Status) {
			return errors.New("stop the experiment before deleting it")
		}
		for _, tx := range s.Transactions {
			if tx.ExperimentID == experimentID && activeTransaction(tx.Status) {
				return errors.New("experiment still has active transactions")
			}
		}
		for _, workload := range s.Workloads {
			if workload.ExperimentID == experimentID && (workload.Status == "queued" || workload.Status == "running" || workload.Status == "draining") {
				return errors.New("experiment still has an active workload")
			}
		}
		s.Experiments = append(s.Experiments[:index], s.Experiments[index+1:]...)
		s.Transactions = deleteTransactionsForExperiment(s.Transactions, experimentID)
		s.Workloads = deleteWorkloadsForExperiment(s.Workloads, experimentID)
		s.AccountSnapshots = deleteSnapshotsForExperiment(s.AccountSnapshots, experimentID)
		s.Events = deleteEventsForExperiment(s.Events, experimentID)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		fail(w, http.StatusNotFound, errors.New("experiment not found"))
		return
	}
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func deleteTransactionsForExperiment(items []model.Transaction, experimentID string) []model.Transaction {
	return slices.DeleteFunc(items, func(item model.Transaction) bool { return item.ExperimentID == experimentID })
}

func deleteWorkloadsForExperiment(items []model.Workload, experimentID string) []model.Workload {
	return slices.DeleteFunc(items, func(item model.Workload) bool { return item.ExperimentID == experimentID })
}

func deleteSnapshotsForExperiment(items []model.AccountSnapshot, experimentID string) []model.AccountSnapshot {
	return slices.DeleteFunc(items, func(item model.AccountSnapshot) bool { return item.ExperimentID == experimentID })
}

func deleteEventsForExperiment(items []model.Event, experimentID string) []model.Event {
	return slices.DeleteFunc(items, func(item model.Event) bool { return item.ExperimentID == experimentID })
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
	request, _ := http.NewRequest(http.MethodGet, "http://lab.local/report?format=json", nil)
	a.exportResults(w, request, eid, "")
}

func (a *API) setExperiment(id, status, errText string, nodes []model.Node, sha string) error {
	return a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			if s.Experiments[i].ID == id {
				s.Experiments[i].Status = status
				s.Experiments[i].Error = errText
				if status == "deploying" {
					s.Experiments[i].MiningStatus, s.Experiments[i].MiningError = "", ""
					s.Experiments[i].MiningUpdatedAt = time.Now()
				}
				if nodes != nil {
					s.Experiments[i].Nodes = nodes
				}
				if sha != "" {
					if sha != s.Experiments[i].ArtifactSHA {
						// A recovery hotfix belongs to one deployed runtime/key set.
						// Never carry it over to a later deployment or key rotation.
						s.Experiments[i].RecoveryArtifactSHA = ""
					}
					s.Experiments[i].ArtifactSHA = sha
				}
				if status == "running" {
					if s.Experiments[i].StartedAt.IsZero() {
						s.Experiments[i].StartedAt = time.Now()
					}
					s.Experiments[i].FinishedAt = time.Time{}
				}
				if status == "stopped" || status == "failed" {
					s.Experiments[i].FinishedAt = time.Now()
				}
				if status == "stopped" {
					for j := range s.Experiments[i].Nodes {
						s.Experiments[i].Nodes[j].Status = "stopped"
						s.Experiments[i].Nodes[j].Peers = 0
						s.Experiments[i].Nodes[j].Mining = nil
					}
				}
			}
		}
		return nil
	})
}
func (a *API) deploy(id string, e model.Experiment, servers map[string]model.Server) {
	admitted := e
	admitted.Nodes = append([]model.Node(nil), e.Nodes...)
	a.emit(id, "info", "lifecycle", "deployment started", nil)
	ctx, cancel := context.WithTimeout(context.Background(), orchestrator.DeploymentTimeout(e))
	defer cancel()
	nodes, err := a.orch.Deploy(ctx, &e, servers, func(l, k, m string, f map[string]any) { a.emit(id, l, k, m, f) })
	if err != nil {
		var preflightErr *orchestrator.DeploymentPreflightError
		if errors.As(err, &preflightErr) {
			if restoreErr := a.restoreDeploymentDraft(admitted, err); restoreErr != nil {
				a.emit(id, "error", "preflight-failed", fmt.Sprintf("部署预检失败且未执行 worker 写入；保留当前实验状态，请核验：%v；%v", err, restoreErr), nil)
			} else {
				a.emit(id, "warning", "preflight-failed", "部署预检失败，已回到可重试草稿："+err.Error(), nil)
			}
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), orchestrator.StopTimeout(e))
		results, cleanupErr := a.orch.StopNodes(cleanupCtx, e, servers, func(l, k, m string, f map[string]any) {
			a.emit(id, l, k, m, f)
		})
		cleanupCancel()
		if cleanupErr != nil {
			err = fmt.Errorf("%w; cleanup after failed deployment also failed: %v", err, cleanupErr)
		} else {
			a.emit(id, "info", "deploy", "partial deployment rolled back", nil)
		}
		_ = a.completeExperimentStop(id, results, cleanupErr, err.Error())
		a.emit(id, "error", "deploy", err.Error(), nil)
		return
	}
	if err := a.setExperiment(id, "running", "", nodes, e.ArtifactSHA); err != nil {
		a.emit(id, "error", "lifecycle", "节点已启动但状态保存失败，修复存储后请重新核验；不要重复部署", nil)
		return
	}
	a.emit(id, "info", "lifecycle", "experiment running", map[string]any{"nodes": len(nodes)})
	go a.monitor(id)
}

func (a *API) monitor(id string) {
	go a.monitorLane(id, true)
	a.monitorLane(id, false)
}

func (a *API) monitorLane(id string, proving bool) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		var exp model.Experiment
		var nodes []model.Node
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
			nodes = monitorLaneNodes(s, exp, proving)
		})
		if !running {
			return
		}
		concurrency := monitorNodeConcurrency
		if proving {
			concurrency = 2
		}
		monitorNodeBatchesLimit(nodes, concurrency, func(ctx context.Context, node model.Node, commit func(func(*model.State) error) error) {
			// Admission can move a node into proof generation while it waits in
			// this round's queue. Recheck before starting an expensive RPC.
			matchesLane := false
			a.store.View(func(s model.State) {
				matchesLane = monitorNodeProving(s, exp.ID, node.ID) == proving
			})
			if !matchesLane {
				return
			}
			_ = a.sampleNodeCommit(ctx, exp, node, servers[node.ServerID], "monitor", commit)
		}, a.store.Update)
	}
}

func (a *API) sampleNode(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, reason string) error {
	return a.sampleNodeCommit(ctx, exp, node, server, reason, a.store.Update)
}

func (a *API) sampleNodeCommit(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, reason string, commit func(func(*model.State) error) error) error {
	started := time.Now()
	expr := `(function(){var z=null,e="";try{z=eth.getAccountState()}catch(x){e=x.toString()}return JSON.stringify({block:eth.blockNumber.toString(),peers:net.peerCount.toString(),mining:eth.mining,account:eth.accounts[0],publicBalance:eth.getBalance(eth.accounts[0]).toString(10),zk:z,zkError:e})})()`
	out, err := a.orch.Attach(ctx, exp, node, server, expr)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("节点状态查询超时，请检查 SSH、IPC 和服务器负载；这不代表进程已退出：%w", ctx.Err())
		}
		if detail := strings.TrimSpace(out); detail != "" && !strings.Contains(err.Error(), detail) {
			err = fmt.Errorf("%w: %s", err, detail)
		}
		if preserved, saveErr := a.preserveBusyMonitorTimeout(ctx, exp.ID, node.ID, reason, err.Error(), started); saveErr != nil {
			return fmt.Errorf("%w; 保存节点查询超时失败：%v", err, saveErr)
		} else if preserved {
			return err
		}
		a.setNodeSampleError(exp.ID, node.ID, err.Error(), reason, started)
		return err
	}
	raw, err := orchestrator.ExtractJSONString(out)
	if err != nil {
		a.setNodeSampleError(exp.ID, node.ID, err.Error(), reason, started)
		return err
	}
	var sample struct {
		Mining        *bool  `json:"mining"`
		Block         string `json:"block"`
		Peers         string `json:"peers"`
		Account       string `json:"account"`
		PublicBalance string `json:"publicBalance"`
		ZK            *struct {
			Balance         string `json:"balance"`
			Commitment      string `json:"commitment"`
			LastTxBlock     string `json:"lastTxBlockNumber"`
			CommitmentReady *bool  `json:"commitmentReady"`
		} `json:"zk"`
		ZKError string `json:"zkError"`
	}
	if err = json.Unmarshal([]byte(raw), &sample); err != nil {
		a.setNodeSampleError(exp.ID, node.ID, err.Error(), reason, started)
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
	var privacyErr error
	a.store.View(func(s model.State) {
		current := node
		for _, e := range s.Experiments {
			if e.ID == exp.ID {
				for _, n := range e.Nodes {
					if n.ID == node.ID {
						current = n
					}
				}
			}
		}
		if privateAccountInitialized(s.Transactions, exp.ID, current) && (sample.ZK == nil || !privateStateOnChain(sample.ZK.LastTxBlock) || (sample.ZK.CommitmentReady != nil && !*sample.ZK.CommitmentReady) || (current.RecoveryWarning != "" && sample.ZK.CommitmentReady == nil)) {
			privacyErr = errors.New(unconfirmedPrivateStateMessage)
		}
	})
	now := time.Now()
	err = commit(func(s *model.State) error {
		for i := range s.Experiments {
			if s.Experiments[i].ID != exp.ID {
				continue
			}
			for j := range s.Experiments[i].Nodes {
				n := &s.Experiments[i].Nodes[j]
				if n.ID != node.ID {
					continue
				}
				if obsoleteNodeSample(s.Experiments[i], *n, started, reason) {
					continue
				}
				oldBalance, oldCommitment, firstSample := n.ZKBalance, n.Commitment, n.LastSeen.IsZero()
				if reason != "recovery" {
					n.Status = "running"
				}
				n.Block = block
				n.Peers = peers
				if s.Experiments[i].MiningStatus != "updating" && !s.Experiments[i].MiningUpdatedAt.After(started) {
					n.Mining = sample.Mining
				}
				n.Account = sample.Account
				n.PublicBalance = sample.PublicBalance
				n.StateError = sample.ZKError
				n.PrivateStateError = ""
				if privacyErr != nil {
					n.PrivateStateError = privacyErr.Error()
				}
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
	if err != nil {
		return err
	}
	return privacyErr
}

func (a *API) setNodeError(experimentID, nodeID, status, message string) {
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			if s.Experiments[i].ID == experimentID {
				for j := range s.Experiments[i].Nodes {
					if s.Experiments[i].Nodes[j].ID == nodeID {
						if s.Experiments[i].Status != "running" || s.Experiments[i].Nodes[j].Status == "recovering" {
							continue
						}
						s.Experiments[i].Nodes[j].Status = status
						s.Experiments[i].Nodes[j].StateError = message
						s.Experiments[i].Nodes[j].Mining = nil
					}
				}
			}
		}
		return nil
	})
}
func (a *API) stop(id string, e model.Experiment, servers map[string]model.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), orchestrator.StopTimeout(e))
	defer cancel()
	results, err := a.orch.StopNodes(ctx, e, servers, func(l, k, m string, f map[string]any) { a.emit(id, l, k, m, f) })
	_ = a.completeExperimentStop(id, results, err, "")
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
	// Only request fields may cross admission; execution evidence is generated
	// by the controller, never accepted from a client-provided transaction.
	t = model.Transaction{ID: id("tx"), ExperimentID: t.ExperimentID, Type: t.Type, FromNode: t.FromNode, ToNode: t.ToNode, Value: t.Value, Status: "queued", SubmittedAt: time.Now()}
	if t.Type != "transfer" && t.Type != "public" {
		t.ToNode = ""
	}
	if err := a.enqueueTransaction(t); err != nil {
		if a.store.Health().Degraded {
			_ = a.finishTx(t.ID, "failed", "", fmt.Errorf("交易入队保存未完成，未执行 RPC：%w", err))
		}
		fail(w, http.StatusConflict, err)
		return
	}
	go a.runTransaction(t)
	jsonOut(w, 202, t)
}

func activeTransaction(status string) bool {
	return status == "queued" || status == "proving" || status == "submitted" || status == "unknown" || status == "settling"
}

var errNodeBusy = errors.New("node already has an active transaction")

func transactionNodesBusy(transactions []model.Transaction, fromNode, toNode string) bool {
	for _, tx := range transactions {
		if !activeTransaction(tx.Status) {
			continue
		}
		if tx.FromNode == fromNode || tx.ToNode == fromNode || (toNode != "" && (tx.FromNode == toNode || tx.ToNode == toNode)) {
			return true
		}
	}
	return false
}

func (a *API) enqueueTransaction(t model.Transaction) error {
	return a.store.Update(func(s *model.State) error {
		for _, run := range s.Workloads {
			if run.Strategy == "ready-pool" && run.ExperimentID == t.ExperimentID && runActive(run) {
				return errors.New("完整实验流程运行中，不能插入手动交易；请先停止新增投递并收尾")
			}
		}
		if err := transactionNodesAvailable(*s, t); err != nil {
			return err
		}
		if t.Type == "createAccount" {
			for _, experiment := range s.Experiments {
				if experiment.ID != t.ExperimentID {
					continue
				}
				for _, node := range experiment.Nodes {
					if node.ID == t.FromNode && privateAccountInitialized(s.Transactions, t.ExperimentID, node) {
						return errors.New("selected node already has a private account; CreateAccount must only run once")
					}
				}
			}
		}
		if transactionNodesBusy(s.Transactions, t.FromNode, t.ToNode) {
			return errors.New("selected node already has an active transaction; wait for it to finish")
		}
		s.Transactions = append(s.Transactions, t)
		return nil
	})
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
		if t.RunPhase != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			_ = a.checkRunReadiness(ctx, t.ID)
			cancel()
		}
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	var exp model.Experiment
	var node model.Node
	var server model.Server
	var toNode model.Node
	privateAccountReady := false
	ok := false
	queued := false
	a.store.View(func(s model.State) {
		for _, current := range s.Transactions {
			if current.ID == t.ID {
				queued = current.Status == "queued"
			}
		}
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
		privateAccountReady = privateAccountInitialized(s.Transactions, t.ExperimentID, node)
	})
	if !queued {
		return
	}
	if t.WorkloadID != "" && (node.IsMiner || (node.Mining != nil && *node.Mining) || ((t.Type == "transfer" || t.Type == "public") && (toNode.IsMiner || (toNode.Mining != nil && *toNode.Mining)))) {
		a.finishTx(t.ID, "failed", "", errors.New("节点角色已变化：矿工不参与自动交易，本次交易未发送"))
		return
	}
	if !ok {
		a.finishTx(t.ID, "failed", "", errors.New("experiment, node or server not found"))
		return
	}
	if t.Type == "createAccount" && t.BatchID != "" {
		lockAny, _ := a.serverSetupLocks.LoadOrStore(server.ID, &sync.Mutex{})
		serverLock := lockAny.(*sync.Mutex)
		serverLock.Lock()
		defer serverLock.Unlock()
		// Another node on this server may have spent minutes generating its
		// proof. Refresh lifecycle and account state after that wait so a stop
		// request or an already completed initialization is still respected.
		a.store.View(func(s model.State) {
			for _, currentExperiment := range s.Experiments {
				if currentExperiment.ID != t.ExperimentID {
					continue
				}
				exp = currentExperiment
				for _, currentNode := range currentExperiment.Nodes {
					if currentNode.ID == t.FromNode {
						node = currentNode
					}
				}
			}
			privateAccountReady = privateAccountInitialized(s.Transactions, t.ExperimentID, node)
		})
	}
	if exp.Status != "running" {
		a.finishTx(t.ID, "failed", "", errors.New("experiment is not running"))
		return
	}
	if node.Status != "running" || (t.Type == "transfer" && toNode.Status != "running") {
		a.finishTx(t.ID, "failed", "", errors.New("节点不可达或正在恢复，本次交易未发送；请恢复节点后重试"))
		return
	}
	if t.Type != "public" && (node.PrivateStateError != "" || (t.Type == "transfer" && toNode.PrivateStateError != "")) {
		a.finishTx(t.ID, "failed", "", errors.New("节点隐私账户状态未恢复，本次交易未发送，请先恢复节点"))
		return
	}
	if t.Type == "createAccount" && privateAccountReady {
		a.finishTx(t.ID, "skipped", "", errors.New("private account is already initialized; duplicate CreateAccount was skipped"))
		return
	}
	value := t.Value
	if value == "" {
		value = "0x1"
	}
	if t.Type != "createAccount" {
		canonical, err := transactionRPCAmount(t.Type, value)
		if err != nil {
			_ = a.finishTx(t.ID, "failed", "", err)
			return
		}
		value = canonical
	}
	expr := ""
	if t.Type == "transfer" {
		if err := a.setTxCommand(t.ID, transferCommand(node.Name, toNode.Name, value)); err != nil {
			_ = a.finishTx(t.ID, "failed", "", fmt.Errorf("交易指令保存失败，未发送交易：%w", err))
			return
		}
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
	if err := a.setTxCommand(t.ID, node.Name+": "+expr); err != nil {
		_ = a.finishTx(t.ID, "failed", "", fmt.Errorf("交易指令保存失败，未发送交易：%w", err))
		return
	}
	if err := a.beginTransactionRPC(t.ID, "submit"); err != nil {
		_ = a.finishTx(t.ID, "failed", "", fmt.Errorf("未发送交易：%w", err))
		return
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := a.orch.Attach(ctx, exp, node, server, expr)
	rpcWallUs := time.Since(started).Microseconds()
	if err != nil {
		var notExecuted *orchestrator.PreExecutionAttachError
		if errors.As(err, &notExecuted) {
			_ = a.finishTx(t.ID, "failed", "", fmt.Errorf("控制台初始化失败，交易指令未执行：%w", err))
			return
		}
		_ = a.markTransactionUnknown(t.ID, orchestrator.ParseHash(out), fmt.Errorf("RPC 返回失败：%w: %s", err, out))
		return
	}
	hash := orchestrator.ParseHash(out)
	if hash == "" {
		message := strings.TrimSpace(out)
		if message == "" {
			message = "node returned no transaction hash"
		}
		_ = a.markTransactionUnknown(t.ID, "", fmt.Errorf("RPC 未返回可确认的交易哈希：%s", message))
		return
	}
	// Save the hash before timing lookups or receipt polling can be interrupted.
	if err := a.updateTx(t.ID, "submitted", hash, ""); err != nil {
		_ = a.markTransactionUnknown(t.ID, hash, err)
		return
	}
	proofMs := rpcWallUs / 1000
	proofUs := rpcWallUs
	parsedProofUs, parsedVerifyUs := orchestrator.ParseProofTimesMicros(out)
	if t.Type == "public" {
		// A public transfer has no ZK proof stage. Its entire RPC wall time is
		// transaction generation and must not leak into proof metrics.
		proofUs, parsedVerifyUs = 0, 0
	} else if logProofUs, logVerifyUs, timingErr := a.orch.TransactionTimings(ctx, exp, node, server, hash); timingErr == nil {
		parsedProofUs, parsedVerifyUs = logProofUs, logVerifyUs
		if parsedProofUs > 0 {
			proofUs = parsedProofUs
		}
	} else if parsedProofUs > 0 {
		proofUs = parsedProofUs
	}
	txGenerationUs := rpcWallUs - proofUs - parsedVerifyUs
	if txGenerationUs < 0 {
		txGenerationUs = 0
	}
	a.setTransactionBreakdown(t.ID, map[string]int64{"txGenerationUs": txGenerationUs})
	proofMs = proofUs / 1000
	a.emit(t.ExperimentID, "info", "transaction", "transaction submitted", map[string]any{"id": t.ID, "hash": hash, "proofDurationMs": proofMs})
	for i := 0; i < 300 && ctx.Err() == nil; i++ {
		out, err = a.orch.Attach(ctx, exp, node, server, "JSON.stringify(eth.getTransactionReceipt("+strconv.Quote(hash)+"))")
		if err == nil {
			if receipt, block, receiptStatus, ok := parseReceipt(out); ok {
				if _, verificationUs, timingErr := a.orch.TransactionPhaseTimings(ctx, exp, node, server, hash); timingErr == nil {
					// Generation is measured from the RPC wall time with proof and
					// proof verification removed. The node's legacy Create marker
					// includes proof generation, so it must not overwrite that value.
					a.setTransactionBreakdown(t.ID, map[string]int64{"txVerificationUs": verificationUs})
				}
				if err := a.confirmTx(t.ID, hash, receipt, block, receiptStatus, proofUs, parsedVerifyUs); err != nil {
					_ = a.markTransactionUnknown(t.ID, hash, err)
					return
				}
				a.refreshTransactionNodes(exp, node, toNode, server, t.ID)
				if t.Type != "public" {
					a.waitForNextBlock(ctx, exp, node, server, block)
				}
				return
			}
		}
		time.Sleep(time.Second)
	}
	_ = a.markTransactionUnknown(t.ID, hash, errors.New("未在查询窗口内观察到 Receipt"))
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
	if err := a.beginTransactionRPC(t.ID, "payer-proof"); err != nil {
		_ = a.finishTx(t.ID, "failed", "", fmt.Errorf("未开始付款方证明：%w", err))
		return
	}
	started := time.Now()
	out, err := a.orch.Attach(ctx, exp, payer, payerServer, "JSON.stringify(eth.getPayerNextState('0x01',"+strconv.Quote(value)+"))")
	payerWallUs := time.Since(started).Microseconds()
	if err != nil {
		var notExecuted *orchestrator.PreExecutionAttachError
		if errors.As(err, &notExecuted) {
			_ = a.finishTx(t.ID, "failed", "", fmt.Errorf("控制台初始化失败，付款方证明指令未执行：%w", err))
			return
		}
		_ = a.markTransactionUnknown(t.ID, "", fmt.Errorf("付款方证明调用结果不明：%w: %s", err, out))
		return
	}
	raw, extractErr := orchestrator.ExtractJSONString(out)
	if extractErr != nil {
		_ = a.markTransactionUnknown(t.ID, "", extractErr)
		return
	}
	var proof struct {
		CMT   string `json:"cmtANew"`
		SN    string `json:"snAOld"`
		Proof string `json:"proofA"`
		Root  string `json:"proofRoot"`
		Block string `json:"proofBlock"`
	}
	if err := json.Unmarshal([]byte(raw), &proof); err != nil || proof.Proof == "" || proof.Root == "" || proof.Block == "" {
		_ = a.markTransactionUnknown(t.ID, "", fmt.Errorf("付款方证明响应不完整（必须包含 proofA、proofRoot、proofBlock），请确认所有节点运行包版本一致；付款状态可能已冻结，未自动重发：%v", err))
		return
	}
	expr := "eth.sendTransferTransaction({from:eth.accounts[0],value:" + strconv.Quote(value) + ",rs:'0x01',cmtANew:" + strconv.Quote(proof.CMT) + ",snAOld:" + strconv.Quote(proof.SN) + ",proofA:" + strconv.Quote(proof.Proof) + ",proofRoot:" + strconv.Quote(proof.Root) + ",proofBlock:" + strconv.Quote(proof.Block) + "})"
	payerProofUs, payerVerifyUs := orchestrator.ParseProofTimesMicros(out)
	if logProofUs, logVerifyUs, timingErr := a.orch.RecentProofTimings(ctx, exp, payer, payerServer); timingErr == nil {
		payerProofUs, payerVerifyUs = logProofUs, logVerifyUs
	}
	if err := a.beginTransferReceiverRPC(t.ID, proof.CMT); err != nil {
		_ = a.markTransactionUnknown(t.ID, "", fmt.Errorf("付款方已生成状态，接收方提交未执行：%w", err))
		return
	}
	receiverStarted := time.Now()
	out, err = a.orch.Attach(ctx, exp, receiver, receiverServer, expr)
	receiverWallUs := time.Since(receiverStarted).Microseconds()
	if err != nil {
		_ = a.markTransactionUnknown(t.ID, orchestrator.ParseHash(out), fmt.Errorf("接收方提交结果不明，未回滚或重发：%w: %s", err, out))
		return
	}
	hash := orchestrator.ParseHash(out)
	if hash == "" {
		message := strings.TrimSpace(out)
		if message == "" {
			message = "node returned no transaction hash"
		}
		_ = a.markTransactionUnknown(t.ID, "", fmt.Errorf("接收方未返回可确认的哈希，未回滚或重发：%s", message))
		return
	}
	if err := a.updateTx(t.ID, "submitted", hash, ""); err != nil {
		_ = a.markTransactionUnknown(t.ID, hash, err)
		return
	}
	receiverProofUs, receiverVerifyUs := orchestrator.ParseProofTimesMicros(out)
	if logProofUs, logVerifyUs, timingErr := a.orch.TransactionTimings(ctx, exp, receiver, receiverServer, hash); timingErr == nil {
		receiverProofUs, receiverVerifyUs = logProofUs, logVerifyUs
	}
	proofUs := payerProofUs + receiverProofUs
	if proofUs == 0 {
		proofUs = time.Since(started).Microseconds()
	}
	verifyUs := payerVerifyUs + receiverVerifyUs
	payerTxGenerationUs := payerWallUs - payerProofUs - payerVerifyUs
	receiverTxGenerationUs := receiverWallUs - receiverProofUs - receiverVerifyUs
	if payerTxGenerationUs < 0 {
		payerTxGenerationUs = 0
	}
	if receiverTxGenerationUs < 0 {
		receiverTxGenerationUs = 0
	}
	a.setTransactionBreakdown(t.ID, map[string]int64{"payerProofGenerationUs": payerProofUs, "payerProofVerificationUs": payerVerifyUs, "payerTxGenerationUs": payerTxGenerationUs, "receiverProofGenerationUs": receiverProofUs, "receiverProofVerificationUs": receiverVerifyUs, "receiverTxGenerationUs": receiverTxGenerationUs, "txGenerationUs": payerTxGenerationUs + receiverTxGenerationUs})
	for i := 0; i < 600 && ctx.Err() == nil; i++ {
		out, err = a.orch.Attach(ctx, exp, receiver, receiverServer, "JSON.stringify(eth.getTransactionReceipt("+strconv.Quote(hash)+"))")
		if err == nil {
			if receipt, block, receiptStatus, ok := parseReceipt(out); ok {
				phaseValues := map[string]int64{}
				if _, verificationUs, timingErr := a.orch.TransactionPhaseTimings(ctx, exp, receiver, receiverServer, hash); timingErr == nil {
					phaseValues["receiverTxVerificationUs"] = verificationUs
					phaseValues["txVerificationUs"] = verificationUs
				}
				if _, verificationUs, timingErr := a.orch.TransactionPhaseTimings(ctx, exp, payer, payerServer, hash); timingErr == nil {
					phaseValues["payerTxVerificationUs"] = verificationUs
				}
				a.setTransactionBreakdown(t.ID, phaseValues)
				if err := a.confirmTx(t.ID, hash, receipt, block, receiptStatus, proofUs, verifyUs); err != nil {
					_ = a.markTransactionUnknown(t.ID, hash, err)
					return
				}
				a.refreshTransactionNodes(exp, payer, receiver, payerServer, t.ID)
				if t.RunPhase == "" {
					a.waitForNextBlock(ctx, exp, receiver, receiverServer, block)
				}
				return
			}
		}
		time.Sleep(time.Second)
	}
	_ = a.markTransactionUnknown(t.ID, hash, errors.New("未在查询窗口内观察到 Transfer Receipt"))
}

func privateStateOnChain(block string) bool {
	block = strings.TrimSpace(strings.ToLower(block))
	return block != "" && block != "0" && block != "0x0"
}

func privateAccountInitialized(transactions []model.Transaction, experimentID string, node model.Node) bool {
	if node.ID == "" {
		return false
	}
	if privateStateOnChain(node.LastTxBlock) {
		return true
	}
	for _, tx := range transactions {
		if tx.ExperimentID == experimentID && tx.FromNode == node.ID && tx.Type == "createAccount" && tx.Status == "confirmed" {
			return true
		}
	}
	return false
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
	if _, valid := parseBlockValue(block); !valid || (status != "0x0" && status != "0" && status != "false" && status != "0x1" && status != "1" && status != "true") {
		return "", "", "", false
	}
	return raw, block, status, true
}

func (a *API) confirmTx(id, hash, receipt, block, receiptStatus string, proofUs, verifyUs int64) error {
	finalStatus := "confirmed"
	var txErr error
	if receiptStatus == "0" || receiptStatus == "0x0" || receiptStatus == "false" {
		finalStatus = "failed"
		txErr = errors.New("transaction reverted on chain")
	}
	return a.store.Update(func(s *model.State) error {
		for j := range s.Transactions {
			if s.Transactions[j].ID == id {
				if s.Transactions[j].RunPhase != "" && s.Transactions[j].ReadyAt.IsZero() {
					finalStatus = "settling"
				}
				s.Transactions[j].Status, s.Transactions[j].Hash = finalStatus, hash
				s.Transactions[j].ConfirmedAt = time.Now()
				s.Transactions[j].Error, s.Transactions[j].ReconciliationError = "", ""
				if txErr != nil {
					s.Transactions[j].Error = txErr.Error()
				}
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

func (a *API) setTransactionBreakdown(id string, values map[string]int64) {
	_ = a.store.Update(func(s *model.State) error {
		for i := range s.Transactions {
			tx := &s.Transactions[i]
			if tx.ID != id {
				continue
			}
			if v, ok := values["txGenerationUs"]; ok {
				tx.TxGenerationUs = v
			}
			if v, ok := values["txVerificationUs"]; ok {
				tx.TxVerificationUs = v
			}
			if v, ok := values["payerProofGenerationUs"]; ok {
				tx.PayerProofGenerationUs = v
			}
			if v, ok := values["payerProofVerificationUs"]; ok {
				tx.PayerProofVerificationUs = v
			}
			if v, ok := values["payerTxGenerationUs"]; ok {
				tx.PayerTxGenerationUs = v
			}
			if v, ok := values["payerTxVerificationUs"]; ok {
				tx.PayerTxVerificationUs = v
			}
			if v, ok := values["receiverProofGenerationUs"]; ok {
				tx.ReceiverProofGenerationUs = v
			}
			if v, ok := values["receiverProofVerificationUs"]; ok {
				tx.ReceiverProofVerificationUs = v
			}
			if v, ok := values["receiverTxGenerationUs"]; ok {
				tx.ReceiverTxGenerationUs = v
			}
			if v, ok := values["receiverTxVerificationUs"]; ok {
				tx.ReceiverTxVerificationUs = v
			}
		}
		return nil
	})
}

func (a *API) refreshTransactionNodes(exp model.Experiment, from, to model.Node, fromServer model.Server, txID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.refreshTransactionNodesContext(ctx, exp, from, to, fromServer, txID)
}

func (a *API) refreshTransactionNodesContext(ctx context.Context, exp model.Experiment, from, to model.Node, fromServer model.Server, txID string) {
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
	if v.Strategy == "ready-pool" {
		a.createRun(w, r, v)
		return
	}
	if v.ExperimentID == "" || v.Type == "" || v.RatePerSecond < 0.01 || v.RatePerSecond > 100 || v.DurationSeconds < 1 || v.DurationSeconds > 7*24*3600 {
		fail(w, 400, errors.New("experimentId, type, ratePerSecond in [0.01,100], and durationSeconds in [1,604800] are required"))
		return
	}
	if !map[string]bool{"mint": true, "transfer": true, "redeem": true, "public": true}[v.Type] {
		fail(w, 400, errors.New("unsupported workload transaction type"))
		return
	}
	if v.Strategy == "" {
		v.Strategy = "round-robin"
	}
	if v.Strategy != "round-robin" {
		fail(w, http.StatusBadRequest, errors.New("unsupported workload strategy"))
		return
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
	v.StartedAt, v.FinishedAt, v.SubmissionStoppedAt = time.Time{}, time.Time{}, time.Time{}
	v.Submitted, v.Attempted, v.SkippedBusy, v.SkippedUnavailable = 0, 0, 0, 0
	v.StopRequested, v.Error = false, ""
	if err := a.store.Update(func(s *model.State) error {
		for _, existing := range s.Workloads {
			if existing.ExperimentID == v.ExperimentID && existing.Strategy == "ready-pool" && (existing.Status == "queued" || existing.Status == "running" || existing.Status == "draining") {
				return errors.New("完整实验流程正在占用该实验，请先停止并收尾")
			}
		}
		for _, exp := range s.Experiments {
			if exp.ID != v.ExperimentID {
				continue
			}
			if exp.Status != "running" || len(exp.Nodes) == 0 {
				return errors.New("experiment is not running or has no nodes")
			}
			if (v.Type == "transfer" || v.Type == "public") && len(exp.Nodes) < 2 {
				return errors.New("transfer/public workload requires at least two nodes")
			}
			s.Workloads = append(s.Workloads, v)
			return nil
		}
		return os.ErrNotExist
	}); err != nil {
		code := http.StatusConflict
		if a.store.Health().Degraded {
			code = http.StatusServiceUnavailable
		}
		if errors.Is(err, os.ErrNotExist) {
			code = http.StatusNotFound
		}
		fail(w, code, err)
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
	exp.Nodes = slices.DeleteFunc(slices.Clone(exp.Nodes), func(n model.Node) bool { return n.IsMiner || (n.Mining != nil && *n.Mining) })
	if !found || exp.Status != "running" || len(exp.Nodes) == 0 {
		a.finishWorkload(v.ID, "failed", "实验未运行或没有可参与交易的非矿工节点")
		return
	}
	if (v.Type == "transfer" || v.Type == "public") && len(exp.Nodes) < 2 {
		a.finishWorkload(v.ID, "failed", "自动转账至少需要两个非矿工节点")
		return
	}
	if err := a.store.Update(func(s *model.State) error {
		for i := range s.Workloads {
			w := &s.Workloads[i]
			if w.ID == v.ID {
				if w.StopRequested || w.Status != "queued" {
					return errWorkloadStopped
				}
				w.Status, w.StartedAt = "running", time.Now()
				return nil
			}
		}
		return os.ErrNotExist
	}); err != nil {
		return
	}
	interval := time.Duration(float64(time.Second) / v.RatePerSecond)
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.NewTimer(time.Duration(v.DurationSeconds) * time.Second)
	defer deadline.Stop()
	stopCheck := time.NewTicker(250 * time.Millisecond)
	defer stopCheck.Stop()
	submitted := 0
	routeCursor := 0
	for {
		select {
		case <-stopCheck.C:
			stopped := false
			a.store.View(func(s model.State) {
				for _, w := range s.Workloads {
					if w.ID == v.ID {
						stopped = w.StopRequested || w.Status != "running"
					}
				}
			})
			if stopped {
				a.drainWorkload(v, submitted)
				return
			}
		case <-deadline.C:
			if err := a.updateWorkload(v.ID, "draining", submitted); err != nil {
				return
			}
			a.emit(v.ExperimentID, "info", "workload", "workload stopped submitting; waiting for transactions", map[string]any{"id": v.ID, "submitted": submitted})
			a.drainWorkload(v, submitted)
			return
		case <-ticker.C:
			node := exp.Nodes[routeCursor%len(exp.Nodes)]
			toNode := ""
			if v.Type == "transfer" || v.Type == "public" {
				if len(exp.Nodes) < 2 {
					a.finishWorkload(v.ID, "failed", "transfer/public workload requires at least two nodes")
					return
				}
				toNode = exp.Nodes[(routeCursor+1)%len(exp.Nodes)].ID
			}
			routeCursor++
			tx := model.Transaction{ID: id("tx"), WorkloadID: v.ID, Sequence: submitted + 1, ExperimentID: v.ExperimentID, Type: v.Type, FromNode: node.ID, ToNode: toNode, Value: v.Value, Status: "queued", SubmittedAt: time.Now()}
			queued, err := a.enqueueWorkloadTick(v.ID, tx)
			if errors.Is(err, errWorkloadStopped) {
				a.drainWorkload(v, submitted)
				return
			}
			if err != nil {
				a.finishWorkload(v.ID, "failed", err.Error())
				return
			}
			if !queued {
				continue
			}
			submitted++
			go a.runTransaction(tx)
		}
	}
}

func (a *API) drainWorkload(v model.Workload, submitted int) {
	l, _ := a.workloadDrainLocks.LoadOrStore(v.ID, &sync.Mutex{})
	if !l.(*sync.Mutex).TryLock() {
		return
	}
	defer l.(*sync.Mutex).Unlock()
	deadline := time.NewTimer(30 * time.Minute)
	ticker := time.NewTicker(time.Second)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			a.finishWorkload(v.ID, "interrupted", "等待已发送交易超过 30 分钟，未确认交易继续待核验；不会自动重发。")
			return
		case <-ticker.C:
			terminal, confirmed, failed := 0, 0, 0
			draining := false
			a.store.View(func(s model.State) {
				for _, w := range s.Workloads {
					if w.ID == v.ID {
						draining = w.Status == "draining"
						submitted = w.Submitted
					}
				}
				for _, tx := range s.Transactions {
					if tx.WorkloadID != v.ID {
						continue
					}
					switch tx.Status {
					case "confirmed":
						confirmed++
						terminal++
					case "failed", "timeout", "cancelled", "skipped":
						failed++
						terminal++
					}
				}
			})
			if !draining {
				return
			}
			if terminal < submitted {
				continue
			}
			status := "completed"
			errText := ""
			a.store.View(func(s model.State) {
				for _, w := range s.Workloads {
					if w.ID == v.ID && w.InvalidReason != "" {
						status = "completed-with-errors"
						errText = w.InvalidReason
					}
				}
			})
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

func (a *API) updateWorkload(id, status string, submitted int) error {
	return a.store.Update(func(s *model.State) error {
		for i := range s.Workloads {
			if s.Workloads[i].ID == id {
				if s.Workloads[i].Status == "interrupted" || s.Workloads[i].Status == "cancelled" {
					return errWorkloadStopped
				}
				s.Workloads[i].Status = status
				if status == "draining" && s.Workloads[i].SubmissionStoppedAt.IsZero() {
					s.Workloads[i].SubmissionStoppedAt = time.Now()
				}
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
func (a *API) finishWorkload(id, status, errText string) error {
	return a.store.Update(func(s *model.State) error {
		for i := range s.Workloads {
			if s.Workloads[i].ID == id {
				if s.Workloads[i].Status == "cancelled" || s.Workloads[i].Status == "interrupted" || s.Workloads[i].Status == "completed" || s.Workloads[i].Status == "completed-with-errors" {
					return nil
				}
				s.Workloads[i].Status = status
				s.Workloads[i].Error = errText
				s.Workloads[i].FinishedAt = time.Now()
				if s.Workloads[i].SubmissionStoppedAt.IsZero() {
					s.Workloads[i].SubmissionStoppedAt = s.Workloads[i].FinishedAt
				}
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
			if t.Type != "public" && proofUs > 0 {
				proofsUs = append(proofsUs, proofUs)
			}
			if t.Type != "public" && verifyUs > 0 {
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
func (a *API) updateTx(id, status, hash, errText string) error {
	return a.store.Update(func(s *model.State) error {
		for i := range s.Transactions {
			if s.Transactions[i].ID == id {
				if s.Transactions[i].Receipt != "" || s.Transactions[i].Status == "confirmed" {
					return nil
				}
				if s.Transactions[i].Status != "unknown" {
					s.Transactions[i].Status, s.Transactions[i].Error = status, errText
				}
				if status == "proving" && s.Transactions[i].ProvingAt.IsZero() {
					s.Transactions[i].ProvingAt = time.Now()
				}
				if status == "submitted" && s.Transactions[i].BroadcastAt.IsZero() {
					s.Transactions[i].BroadcastAt = time.Now()
				}
				if hash != "" {
					s.Transactions[i].Hash = hash
				}
			}
		}
		return nil
	})
}

func (a *API) setTxCommand(id, command string) error {
	return a.store.Update(func(s *model.State) error {
		for i := range s.Transactions {
			if s.Transactions[i].ID == id {
				s.Transactions[i].Command = command
			}
		}
		return nil
	})
}
func (a *API) finishTx(id, status, hash string, err error) error {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	saveErr := a.store.Update(func(s *model.State) error {
		for i := range s.Transactions {
			if s.Transactions[i].ID == id {
				if s.Transactions[i].Status == "unknown" || s.Transactions[i].Status == "cancelled" || s.Transactions[i].Status == "confirmed" || s.Transactions[i].Receipt != "" {
					return nil
				}
				s.Transactions[i].Status = status
				if hash != "" {
					s.Transactions[i].Hash = hash
				}
				s.Transactions[i].Error = msg
				s.Transactions[i].ConfirmedAt = time.Now()
			}
		}
		return nil
	})
	if saveErr != nil {
		a.pendingTxFinishes.Store(id, transactionFinish{status: status, hash: hash, cause: err})
	} else {
		a.pendingTxFinishes.Delete(id)
	}
	return saveErr
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
