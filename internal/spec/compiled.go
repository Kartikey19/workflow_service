package spec

import "github.com/kartikey/workflow_service/internal/core/expr"

// Compiled separates serializable workflow data from reusable in-memory programs.
// Executor-specific validation and hashing must precede publication (not yet wired).
type Compiled struct {
	Spec     Spec     `json:"spec"`
	Programs Programs `json:"-"`
}

type Spec struct {
	APIVersion   string                `json:"api_version"`
	Name         string                `json:"name"`
	Description  string                `json:"description,omitempty"`
	Inputs       map[string]InputField `json:"inputs,omitempty"`
	TimeoutNS    int64                 `json:"timeout_ns"`
	Correlation  string                `json:"correlation,omitempty"`
	Nodes        []NodeSpec            `json:"nodes"`
	Edges        map[string][]string   `json:"edges"`
	ReverseEdges map[string][]string   `json:"reverse_edges"`
	Roots        []string              `json:"roots"`
	TopoOrder    []string              `json:"topo_order"`
	Outputs      map[string]any        `json:"outputs,omitempty"`
}

type NodeSpec struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Needs     []string       `json:"needs"`
	Join      string         `json:"join"`
	If        string         `json:"if,omitempty"`
	Config    map[string]any `json:"config"`
	Retry     RetrySpec      `json:"retry"`
	TimeoutNS *int64         `json:"timeout_ns,omitempty"`
	OnFailure string         `json:"on_failure,omitempty"`
	OnTimeout string         `json:"on_timeout,omitempty"`
}

type RetrySpec struct {
	MaxAttempts int      `json:"max_attempts"`
	Backoff     string   `json:"backoff"`
	InitialNS   int64    `json:"initial_ns"`
	MaxNS       int64    `json:"max_ns"`
	Jitter      float64  `json:"jitter"`
	RetryOn     []string `json:"retry_on,omitempty"`
}

type Programs struct {
	Nodes       map[string]NodePrograms
	Correlation *expr.Template
	Outputs     *expr.Template
}
type NodePrograms struct {
	Config *expr.Template
	If     *expr.Program
}
