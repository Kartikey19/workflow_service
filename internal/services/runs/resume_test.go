package runs

import (
	"slices"
	"testing"

	"github.com/kartikey/workflow_service/internal/spec"
	"github.com/kartikey/workflow_service/internal/store/gen"
)

func compileSpec(t *testing.T, source string) spec.Spec {
	t.Helper()
	compiled, diagnostics := spec.Compile([]byte(source))
	if compiled == nil {
		t.Fatalf("compile: %+v", diagnostics)
	}
	return compiled.Spec
}

func ids(rows []gen.NodeExecution) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.NodeID)
	}
	slices.Sort(out)
	return out
}

func TestResumePlan(t *testing.T) {
	// fetch -> classify -> notify -> report, with an independent audit root
	// that report also needs.
	s := compileSpec(t, `apiVersion: workflow/v1
name: resume-plan
nodes:
  - {id: fetch, type: noop, config: {}}
  - {id: audit, type: noop, config: {}}
  - {id: classify, type: noop, needs: [fetch], config: {}}
  - {id: notify, type: noop, needs: [classify], config: {}}
  - {id: report, type: noop, needs: [audit, notify], config: {}}
`)
	tests := []struct {
		name   string
		latest map[string]string
		reuse  []string
		entry  []string
	}{
		{
			name:   "failed middle step reruns it and everything after",
			latest: map[string]string{"fetch": "succeeded", "audit": "succeeded", "classify": "failed", "notify": "skipped", "report": "skipped"},
			reuse:  []string{"audit", "fetch"},
			entry:  []string{"classify"},
		},
		{
			name:   "cancelled parallel root is scheduled again",
			latest: map[string]string{"fetch": "succeeded", "audit": "cancelled", "classify": "failed"},
			reuse:  []string{"fetch"},
			entry:  []string{"audit", "classify"},
		},
		{
			name:   "a root that never started is scheduled",
			latest: map[string]string{"fetch": "failed"},
			reuse:  []string{},
			entry:  []string{"audit", "fetch"},
		},
		{
			name:   "nothing failed: everything is reused",
			latest: map[string]string{"fetch": "succeeded", "audit": "succeeded", "classify": "succeeded", "notify": "succeeded", "report": "succeeded"},
			reuse:  []string{"audit", "classify", "fetch", "notify", "report"},
			entry:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var latest []gen.NodeExecution
			for _, node := range s.Nodes {
				if status, ok := tc.latest[node.ID]; ok {
					latest = append(latest, gen.NodeExecution{NodeID: node.ID, Status: status})
				}
			}
			reuse, entry := resumePlan(s, latest)
			slices.Sort(entry)
			if got := ids(reuse); !slices.Equal(got, tc.reuse) {
				t.Errorf("reuse = %v, want %v", got, tc.reuse)
			}
			if !slices.Equal(entry, tc.entry) {
				t.Errorf("entry = %v, want %v", entry, tc.entry)
			}
		})
	}
}
