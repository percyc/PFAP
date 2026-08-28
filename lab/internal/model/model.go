package model

import "time"

type Server struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Host           string    `json:"host"`
	P2PHost        string    `json:"p2pHost,omitempty"`
	Port           int       `json:"port"`
	User           string    `json:"user"`
	IdentityFile   string    `json:"identityFile,omitempty"`
	KnownHostsFile string    `json:"knownHostsFile,omitempty"`
	WorkDir        string    `json:"workDir"`
	Labels         []string  `json:"labels,omitempty"`
	Status         string    `json:"status"`
	LastCheck      time.Time `json:"lastCheck,omitempty"`
	SystemInfo     string    `json:"systemInfo,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

type Placement struct {
	ServerID string `json:"serverId"`
	Count    int    `json:"count"`
}

type Experiment struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Status       string      `json:"status"`
	NetworkID    int         `json:"networkId"`
	P2PPortBase  int         `json:"p2pPortBase"`
	RPCPortBase  int         `json:"rpcPortBase"`
	ArtifactPath string      `json:"artifactPath"`
	ArtifactSHA  string      `json:"artifactSha,omitempty"`
	Topology     string      `json:"topology"`
	Placements   []Placement `json:"placements"`
	Nodes        []Node      `json:"nodes,omitempty"`
	CreatedAt    time.Time   `json:"createdAt"`
	StartedAt    time.Time   `json:"startedAt,omitempty"`
	FinishedAt   time.Time   `json:"finishedAt,omitempty"`
	Error        string      `json:"error,omitempty"`
}

type Node struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	ServerID      string    `json:"serverId"`
	Index         int       `json:"index"`
	LocalIndex    int       `json:"localIndex"`
	P2PPort       int       `json:"p2pPort"`
	RPCPort       int       `json:"rpcPort"`
	Status        string    `json:"status"`
	Block         uint64    `json:"block"`
	Peers         int       `json:"peers"`
	Account       string    `json:"account,omitempty"`
	PublicBalance string    `json:"publicBalance,omitempty"`
	ZKBalance     string    `json:"zkBalance,omitempty"`
	Commitment    string    `json:"commitment,omitempty"`
	LastTxBlock   string    `json:"lastTxBlock,omitempty"`
	StateError    string    `json:"stateError,omitempty"`
	LastSeen      time.Time `json:"lastSeen,omitempty"`
}

type Event struct {
	ID           string         `json:"id"`
	ExperimentID string         `json:"experimentId,omitempty"`
	Level        string         `json:"level"`
	Kind         string         `json:"kind"`
	Message      string         `json:"message"`
	Fields       map[string]any `json:"fields,omitempty"`
	At           time.Time      `json:"at"`
}

type Transaction struct {
	ID               string    `json:"id"`
	WorkloadID       string    `json:"workloadId,omitempty"`
	Sequence         int       `json:"sequence,omitempty"`
	ExperimentID     string    `json:"experimentId"`
	Type             string    `json:"type"`
	FromNode         string    `json:"fromNode"`
	ToNode           string    `json:"toNode,omitempty"`
	Value            string    `json:"value,omitempty"`
	Status           string    `json:"status"`
	Hash             string    `json:"hash,omitempty"`
	Error            string    `json:"error,omitempty"`
	Command          string    `json:"command,omitempty"`
	SubmittedAt      time.Time `json:"submittedAt"`
	ProvingAt        time.Time `json:"provingAt,omitempty"`
	BroadcastAt      time.Time `json:"broadcastAt,omitempty"`
	ConfirmedAt      time.Time `json:"confirmedAt,omitempty"`
	ProofDurationMs  int64     `json:"proofDurationMs,omitempty"`
	VerifyDurationMs int64     `json:"verifyDurationMs,omitempty"`
	ProofDurationUs  int64     `json:"proofDurationUs,omitempty"`
	VerifyDurationUs int64     `json:"verifyDurationUs,omitempty"`
	BlockNumber      string    `json:"blockNumber,omitempty"`
	ReceiptStatus    string    `json:"receiptStatus,omitempty"`
	Receipt          string    `json:"receipt,omitempty"`
}

type AccountSnapshot struct {
	ID            string    `json:"id"`
	ExperimentID  string    `json:"experimentId"`
	NodeID        string    `json:"nodeId"`
	Account       string    `json:"account"`
	PublicBalance string    `json:"publicBalance,omitempty"`
	ZKBalance     string    `json:"zkBalance,omitempty"`
	Commitment    string    `json:"commitment,omitempty"`
	LastTxBlock   string    `json:"lastTxBlock,omitempty"`
	ChainBlock    uint64    `json:"chainBlock"`
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
}

type Workload struct {
	ID              string    `json:"id"`
	ExperimentID    string    `json:"experimentId"`
	Name            string    `json:"name"`
	Type            string    `json:"type"`
	Value           string    `json:"value"`
	RatePerSecond   float64   `json:"ratePerSecond"`
	DurationSeconds int       `json:"durationSeconds"`
	Strategy        string    `json:"strategy"`
	Status          string    `json:"status"`
	Submitted       int       `json:"submitted"`
	CreatedAt       time.Time `json:"createdAt"`
	StartedAt       time.Time `json:"startedAt,omitempty"`
	FinishedAt      time.Time `json:"finishedAt,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type State struct {
	Servers          []Server          `json:"servers"`
	Experiments      []Experiment      `json:"experiments"`
	Events           []Event           `json:"events"`
	Transactions     []Transaction     `json:"transactions"`
	Workloads        []Workload        `json:"workloads"`
	AccountSnapshots []AccountSnapshot `json:"accountSnapshots"`
}
