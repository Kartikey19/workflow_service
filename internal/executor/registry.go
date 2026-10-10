package executor

import (
	"fmt"
	"sort"
	"strings"
)

// Description documents a node type for the API, the UI and the workflow
// assistant. Example is a YAML snippet of one node using the type.
type Description struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Category    string `json:"category"`
	Description string `json:"description"`
	Example     string `json:"example"`
	Output      string `json:"output,omitempty"`
}

// Describer is optional; executors without it are listed by type only.
type Describer interface {
	Describe() Description
}

// Registry maps node types to executors. It is built once per process and shared
// by the compiler (ValidateConfig, unknown types) and the dispatcher.
type Registry struct {
	executors map[string]Executor
}

func NewRegistry() *Registry {
	return &Registry{executors: make(map[string]Executor)}
}

func (r *Registry) Register(e Executor) error {
	if e == nil {
		return fmt.Errorf("register executor: executor is nil")
	}
	nodeType := e.Type()
	if strings.TrimSpace(nodeType) == "" {
		return fmt.Errorf("register executor: type is empty")
	}
	if _, exists := r.executors[nodeType]; exists {
		return fmt.Errorf("register executor: duplicate type %q", nodeType)
	}
	r.executors[nodeType] = e
	return nil
}

// MustRegister is for process wiring, where a duplicate type is a programming error.
func (r *Registry) MustRegister(executors ...Executor) {
	for _, e := range executors {
		if err := r.Register(e); err != nil {
			panic(err)
		}
	}
}

func (r *Registry) Get(nodeType string) (Executor, bool) {
	e, exists := r.executors[nodeType]
	return e, exists
}

// Descriptions lists every registered type, sorted by type.
func (r *Registry) Descriptions() []Description {
	out := make([]Description, 0, len(r.executors))
	for nodeType, e := range r.executors {
		d := Description{Type: nodeType}
		if describer, ok := e.(Describer); ok {
			d = describer.Describe()
			d.Type = nodeType
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}
