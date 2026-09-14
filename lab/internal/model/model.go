package model

import "time"

type Server struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Host           string    `json:"host"`
	HostGroup      string    `json:"hostGroup,omitempty"`
	P2PHost        string    `json:"p2pHost,omitempty"`
	Port           int       `json:"port"`
	User           string    `json:"user"`
	IdentityFile   string    `json:"identityFile,omitempty"`
	KnownHostsFile string    `json:"knownHostsFile,omitempty"`
	WorkDir        string    `json:"workDir"`
	Labels         []string  `json:"labels,omitempty"`
	Status         string    `json:"status"`
	LastCheck      time.Time `json:"lastCheck,omitempty"`
	LastSuccessAt  time.Time `json:"lastSuccessAt,omitempty"`
	LastError      string    `json:"lastError,omitempty"`
	SystemInfo     string    `json:"systemInfo,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

type Placement struct {
	ServerID string `json:"serverId"`
	Count    int    `json:"count"`
}

type MinerSelection struct {
	ServerID   string `json:"serverId"`
	LocalIndex int    `json:"localIndex"`
}

type Experiment struct {
	ID                  string           `json:"id"`
	Name                string           `json:"name"`
	Status              string           `json:"status"`
	NetworkID           int              `json:"networkId"`
	P2PPortBase         int              `json:"p2pPortBase"`
	RPCPortBase         int              `json:"rpcPortBase"`
	ArtifactPath        string           `json:"artifactPath"`
	ArtifactSHA         string           `json:"artifactSha,omitempty"`
	RecoveryArtifactSHA string           `json:"recoveryArtifactSha,omitempty"`
	Topology            string           `json:"topology"`
	MinerCount          int              `json:"minerCount"`
	MinerMode           string           `json:"minerMode,omitempty"`
	MinerSelections     []MinerSelection `json:"minerSelections,omitempty"`
	MiningStatus        string           `json:"miningStatus,omitempty"`
	MiningError         string           `json:"miningError,omitempty"`
	MiningUpdatedAt     time.Time        `json:"miningUpdatedAt,omitempty"`
	Placements          []Placement      `json:"placements"`
	Nodes               []Node           `json:"nodes,omitempty"`
	CreatedAt           time.Time        `json:"createdAt"`
	StartedAt           time.Time        `json:"startedAt,omitempty"`
	FinishedAt          time.Time        `json:"finishedAt,omitempty"`
	Error               string           `json:"error,omitempty"`
}

type Node struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	ServerID          string    `json:"serverId"`
	Index             int       `json:"index"`
	LocalIndex        int       `json:"localIndex"`
	P2PPort           int       `json:"p2pPort"`
	RPCPort           int       `json:"rpcPort"`
	Status            string    `json:"status"`
	IsMiner           bool      `json:"isMiner"`
	Mining            *bool     `json:"mining,omitempty"`
	Block             uint64    `json:"block"`
	Peers             int       `json:"peers"`
	Account           string    `json:"account,omitempty"`
	PublicBalance     string    `json:"publicBalance,omitempty"`
	ZKBalance         string    `json:"zkBalance,omitempty"`
	Commitment        string    `json:"commitment,omitempty"`
	LastTxBlock       string    `json:"lastTxBlock,omitempty"`
	StateError        string    `json:"stateError,omitempty"`
	RecoveryError     string    `json:"recoveryError,omitempty"`
	RecoveryWarning   string    `json:"recoveryWarning,omitempty"`
	PrivateStateError string    `json:"privateStateError,omitempty"`
	RuntimeSHA        string    `json:"runtimeSha,omitempty"`
	RecoveryStartedAt time.Time `json:"recoveryStartedAt,omitempty"`
	LastSeen          time.Time `json:"lastSeen,omitempty"`
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
	ID                          string    `json:"id"`
	BatchID                     string    `json:"batchId,omitempty"`
	WorkloadID                  string    `json:"workloadId,omitempty"`
	Sequence                    int       `json:"sequence,omitempty"`
	RunPhase                    string    `json:"runPhase,omitempty"`
	ReadyAt                     time.Time `json:"readyAt,omitempty"`
	ExpectedPayerBalance        string    `json:"expectedPayerBalance,omitempty"`
	ExpectedReceiverBalance     string    `json:"expectedReceiverBalance,omitempty"`
	ExperimentID                string    `json:"experimentId"`
	Type                        string    `json:"type"`
	FromNode                    string    `json:"fromNode"`
	ToNode                      string    `json:"toNode,omitempty"`
	Value                       string    `json:"value,omitempty"`
	Status                      string    `json:"status"`
	Hash                        string    `json:"hash,omitempty"`
	Error                       string    `json:"error,omitempty"`
	ExecutionStage              string    `json:"executionStage,omitempty"`
	SubmissionAttemptedAt       time.Time `json:"submissionAttemptedAt,omitempty"`
	LastCheckedAt               time.Time `json:"lastCheckedAt,omitempty"`
	ReconciliationError         string    `json:"reconciliationError,omitempty"`
	Command                     string    `json:"command,omitempty"`
	SubmittedAt                 time.Time `json:"submittedAt"`
	ProvingAt                   time.Time `json:"provingAt,omitempty"`
	BroadcastAt                 time.Time `json:"broadcastAt,omitempty"`
	ConfirmedAt                 time.Time `json:"confirmedAt,omitempty"`
	ProofDurationMs             int64     `json:"proofDurationMs,omitempty"`
	VerifyDurationMs            int64     `json:"verifyDurationMs,omitempty"`
	ProofDurationUs             int64     `json:"proofDurationUs,omitempty"`
	VerifyDurationUs            int64     `json:"verifyDurationUs,omitempty"`
	TxGenerationUs              int64     `json:"txGenerationUs,omitempty"`
	TxVerificationUs            int64     `json:"txVerificationUs,omitempty"`
	PayerProofGenerationUs      int64     `json:"payerProofGenerationUs,omitempty"`
	PayerProofVerificationUs    int64     `json:"payerProofVerificationUs,omitempty"`
	PayerTxGenerationUs         int64     `json:"payerTxGenerationUs,omitempty"`
	PayerTxVerificationUs       int64     `json:"payerTxVerificationUs,omitempty"`
	ReceiverProofGenerationUs   int64     `json:"receiverProofGenerationUs,omitempty"`
	ReceiverProofVerificationUs int64     `json:"receiverProofVerificationUs,omitempty"`
	ReceiverTxGenerationUs      int64     `json:"receiverTxGenerationUs,omitempty"`
	ReceiverTxVerificationUs    int64     `json:"receiverTxVerificationUs,omitempty"`
	BlockNumber                 string    `json:"blockNumber,omitempty"`
	ReceiptStatus               string    `json:"receiptStatus,omitempty"`
	Receipt                     string    `json:"receipt,omitempty"`
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
	TransferPercent      int            `json:"transferPercent"`
	Configuration        map[string]any `json:"configuration,omitempty"`
	ObserverNodeID       string         `json:"observerNodeId,omitempty"`
	Blocks               []RunBlock     `json:"blocks,omitempty"`
	BlockError           string         `json:"blockError,omitempty"`
	Mode                 string         `json:"mode,omitempty"`
	NodeIDs              []string       `json:"nodeIds,omitempty"`
	WarmupSeconds        int            `json:"warmupSeconds,omitempty"`
	Confirmations        int            `json:"confirmations,omitempty"`
	Phase                string         `json:"phase,omitempty"`
	MeasurementStartedAt time.Time      `json:"measurementStartedAt,omitempty"`
	MeasurementEndsAt    time.Time      `json:"measurementEndsAt,omitempty"`
	InvalidReason        string         `json:"invalidReason,omitempty"`
	ID                   string         `json:"id"`
	ExperimentID         string         `json:"experimentId"`
	Name                 string         `json:"name"`
	Type                 string         `json:"type"`
	Value                string         `json:"value"`
	RatePerSecond        float64        `json:"ratePerSecond"`
	DurationSeconds      int            `json:"durationSeconds"`
	Strategy             string         `json:"strategy"`
	Status               string         `json:"status"`
	Submitted            int            `json:"submitted"`
	Attempted            int            `json:"attempted"`
	SkippedBusy          int            `json:"skippedBusy"`
	SkippedUnavailable   int            `json:"skippedUnavailable"`
	StopRequested        bool           `json:"stopRequested"`
	SubmissionStoppedAt  time.Time      `json:"submissionStoppedAt,omitempty"`
	CreatedAt            time.Time      `json:"createdAt"`
	StartedAt            time.Time      `json:"startedAt,omitempty"`
	FinishedAt           time.Time      `json:"finishedAt,omitempty"`
	Error                string         `json:"error,omitempty"`
}

type RunBlock struct {
	Number       uint64    `json:"number"`
	Hash         string    `json:"hash"`
	ParentHash   string    `json:"parentHash"`
	Timestamp    uint64    `json:"timestamp"`
	Size         uint64    `json:"size"`
	Transactions int       `json:"transactions"`
	ObservedAt   time.Time `json:"observedAt"`
	ValidationUs *int64    `json:"validationUs"`
}

type State struct {
	Servers          []Server          `json:"servers"`
	Experiments      []Experiment      `json:"experiments"`
	Events           []Event           `json:"events"`
	Transactions     []Transaction     `json:"transactions"`
	Workloads        []Workload        `json:"workloads"`
	AccountSnapshots []AccountSnapshot `json:"accountSnapshots"`
}
