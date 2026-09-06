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
	"time"

	"github.com/pfap/lab/internal/model"
	"github.com/pfap/lab/internal/orchestrator"
	"github.com/pfap/lab/internal/store"
)

type API struct {
	store            *store.Store
	orch             orchestrator.Orchestrator
	subscribers      map[chan model.Event]struct{}
	mu               sync.Mutex
	nodeLocks        sync.Map
	serverSetupLocks sync.Map
	lifecycleMu      sync.Mutex
	knownHostsMu     sync.Mutex
}

func New(s *store.Store) *API {
	a := &API{store: s, subscribers: map[chan model.Event]struct{}{}}
	var running []string
	_ = s.Update(func(state *model.State) error {
		migrateMiningState(state)
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
			status := "online"
			if err != nil {
				status = "error"
				out = err.Error()
			}
			_ = a.store.Update(func(s *model.State) error {
				for i := range s.Servers {
					if s.Servers[i].ID == server.ID {
						s.Servers[i].Status = status
						s.Servers[i].SystemInfo = out
						s.Servers[i].LastCheck = time.Now()
					}
				}
				return nil
			})
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
	return status == "running" || status == "deploying" || status == "stopping"
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
		candidate := model.Server{Name: fmt.Sprintf("%s-%02d", prefix, i+1), Host: host, P2PHost: host, Port: request.Port, User: request.User, IdentityFile: request.IdentityFile, WorkDir: request.WorkDir, Status: "unknown", CreatedAt: time.Now()}
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
	if len(parts) == 3 && r.Method == http.MethodPut {
		var updated model.Server
		if err := decode(r, &updated); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
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
		} else {
			updated.Status, updated.LastCheck, updated.SystemInfo = server.Status, server.LastCheck, server.SystemInfo
		}
		err := a.store.Update(func(s *model.State) error {
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
	e.MinerCount = min(2, totalNodes)
	if request.MinerCount != nil {
		e.MinerCount = *request.MinerCount
	}
	if e.MinerCount < 1 || e.MinerCount > totalNodes {
		fail(w, 400, fmt.Errorf("矿工数量必须在 1 到 %d 之间", totalNodes))
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
		a.report(w, eid)
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
	// Serialize lifecycle admission with recovery and mining updates. The
	// accepted state is persisted before launching the background operation.
	if action == "deploy" || action == "start" || action == "stop" {
		a.lifecycleMu.Lock()
		defer a.lifecycleMu.Unlock()
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
	if (action == "deploy" || action == "start") && (exp.Status == "deploying" || exp.Status == "running" || exp.Status == "stopping") {
		fail(w, 409, errors.New("experiment is already active"))
		return
	}
	if exp.MiningStatus == "updating" && (action == "deploy" || action == "start" || action == "stop") {
		fail(w, http.StatusConflict, errors.New("矿工配置正在应用，请等待完成后再操作实验"))
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
		a.setExperiment(eid, "deploying", "", nil, "")
		go a.deploy(eid, exp, servers)
		jsonOut(w, 202, map[string]string{"status": "deploying"})
	case "stop":
		if exp.Status != "running" && exp.Status != "failed" {
			fail(w, http.StatusConflict, errors.New("实验不在可停止状态，请等待当前操作完成"))
			return
		}
		recovering := false
		a.store.View(func(s model.State) {
			for _, e := range s.Experiments {
				if e.ID == eid {
					for _, n := range e.Nodes {
						recovering = recovering || n.Status == "recovering"
					}
				}
			}
		})
		if recovering {
			fail(w, http.StatusConflict, errors.New("节点正在恢复，请等待恢复完成后再停止实验"))
			return
		}
		a.setExperiment(eid, "stopping", "", nil, "")
		go a.stop(eid, exp, servers)
		jsonOut(w, 202, map[string]string{"status": "stopping"})
	case "initialize-accounts":
		a.initializeAccounts(w, eid)
	default:
		fail(w, 404, errors.New("unknown action"))
	}
}

func (a *API) initializeAccounts(w http.ResponseWriter, experimentID string) {
	batchID := id("init")
	queued := []model.Transaction{}
	alreadyInitialized, busy, unavailable, total := 0, 0, 0, 0
	err := a.store.Update(func(s *model.State) error {
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
			if node.Status != "running" {
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
		if exp.Status == "running" || exp.Status == "deploying" || exp.Status == "stopping" {
			return errors.New("stop the experiment before deleting it")
		}
		for _, tx := range s.Transactions {
			if tx.ExperimentID == experimentID && (tx.Status == "queued" || tx.Status == "proving" || tx.Status == "submitted") {
				return errors.New("experiment still has active transactions")
			}
		}
		for _, workload := range s.Workloads {
			if workload.ExperimentID == experimentID && (workload.Status == "running" || workload.Status == "draining") {
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
						s.Experiments[i].Nodes[j].Mining = nil
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
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		cleanupErr := a.orch.Stop(cleanupCtx, e, servers, func(l, k, m string, f map[string]any) {
			a.emit(id, l, k, m, f)
		})
		cleanupCancel()
		if cleanupErr != nil {
			err = fmt.Errorf("%w; cleanup after failed deployment also failed: %v", err, cleanupErr)
		} else {
			a.emit(id, "info", "deploy", "partial deployment rolled back", nil)
		}
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
			if node.Status == "recovering" {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = a.sampleNode(ctx, exp, node, servers[node.ServerID], "monitor")
			cancel()
		}
	}
}

func (a *API) sampleNode(ctx context.Context, exp model.Experiment, node model.Node, server model.Server, reason string) error {
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
		a.setNodeError(exp.ID, node.ID, "unreachable", err.Error())
		return err
	}
	raw, err := orchestrator.ExtractJSONString(out)
	if err != nil {
		a.setNodeError(exp.ID, node.ID, "unreachable", err.Error())
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
			privacyErr = errors.New("已初始化账户未恢复有效的隐私状态或承诺树，已暂停该节点的隐私交易；请检查 SN 读取和承诺树恢复，不要重复 CreateAccount")
		}
	})
	now := time.Now()
	err = a.store.Update(func(s *model.State) error {
		for i := range s.Experiments {
			if s.Experiments[i].ID != exp.ID {
				continue
			}
			for j := range s.Experiments[i].Nodes {
				n := &s.Experiments[i].Nodes[j]
				if n.ID != node.ID {
					continue
				}
				if s.Experiments[i].Status != "running" || n.RecoveryStartedAt.After(started) || (n.Status == "recovering" && reason != "recovery") {
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
	t.BatchID = ""
	t.Status = "queued"
	t.SubmittedAt = time.Now()
	if err := a.enqueueTransaction(t); err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	go a.runTransaction(t)
	jsonOut(w, 202, t)
}

func activeTransaction(status string) bool {
	return status == "queued" || status == "proving" || status == "submitted"
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
		privateAccountReady = privateAccountInitialized(s.Transactions, t.ExperimentID, node)
	})
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
	rpcWallUs := time.Since(started).Microseconds()
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
	a.updateTx(t.ID, "submitted", hash, "")
	a.emit(t.ExperimentID, "info", "transaction", "transaction submitted", map[string]any{"id": t.ID, "hash": hash, "proofDurationMs": proofMs})
	for i := 0; i < 300; i++ {
		out, err = a.orch.Attach(ctx, exp, node, server, "JSON.stringify(eth.getTransactionReceipt("+strconv.Quote(hash)+"))")
		if err == nil {
			if receipt, block, receiptStatus, ok := parseReceipt(out); ok {
				if _, verificationUs, timingErr := a.orch.TransactionPhaseTimings(ctx, exp, node, server, hash); timingErr == nil {
					// Generation is measured from the RPC wall time with proof and
					// proof verification removed. The node's legacy Create marker
					// includes proof generation, so it must not overwrite that value.
					a.setTransactionBreakdown(t.ID, map[string]int64{"txVerificationUs": verificationUs})
				}
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
	payerWallUs := time.Since(started).Microseconds()
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
	if logProofUs, logVerifyUs, timingErr := a.orch.RecentProofTimings(ctx, exp, payer, payerServer); timingErr == nil {
		payerProofUs, payerVerifyUs = logProofUs, logVerifyUs
	}
	receiverStarted := time.Now()
	out, err = a.orch.Attach(ctx, exp, receiver, receiverServer, expr)
	receiverWallUs := time.Since(receiverStarted).Microseconds()
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
	a.updateTx(t.ID, "submitted", hash, "")
	for i := 0; i < 600; i++ {
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
	if !map[string]bool{"mint": true, "transfer": true, "redeem": true, "public": true}[v.Type] {
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
	routeCursor := 0
	for {
		select {
		case <-deadline.C:
			a.updateWorkload(v.ID, "draining", submitted)
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
			if err := a.store.Update(func(s *model.State) error {
				if err := transactionNodesAvailable(*s, tx); err != nil {
					return err
				}
				if transactionNodesBusy(s.Transactions, tx.FromNode, tx.ToNode) {
					return errNodeBusy
				}
				s.Transactions = append(s.Transactions, tx)
				for i := range s.Workloads {
					if s.Workloads[i].ID == v.ID {
						s.Workloads[i].Submitted++
					}
				}
				return nil
			}); err != nil {
				if errors.Is(err, errNodeBusy) || errors.Is(err, errNodeUnavailable) {
					continue
				}
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
