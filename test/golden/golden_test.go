// Package golden_test pins the compiler's output: each feature fixture compiles
// to a checked-in spec, and each rule fixture fails with its exact code.
// Regenerate the expected specs with: go test ./test/golden -update
package golden_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kartikey/workflow_service/internal/executor"
	"github.com/kartikey/workflow_service/internal/executors/core"
	"github.com/kartikey/workflow_service/internal/executors/httpexec"
	"github.com/kartikey/workflow_service/internal/netguard"
	"github.com/kartikey/workflow_service/internal/spec"
)

var update = flag.Bool("update", false, "rewrite the expected .json files from the current compiler")

func registry(t *testing.T) *executor.Registry {
	t.Helper()
	r := executor.NewRegistry()
	r.MustRegister(core.All()...)
	r.MustRegister(httpexec.New(netguard.Policy{}))
	return r
}

type golden struct {
	SpecHash string            `json:"spec_hash"`
	Warnings []spec.Diagnostic `json:"warnings"`
	Spec     spec.Spec         `json:"spec"`
}

func TestGoldenSpecs(t *testing.T) {
	sources, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) == 0 {
		t.Fatal("no golden fixtures found")
	}
	reg := registry(t)
	for _, source := range sources {
		t.Run(strings.TrimSuffix(source, ".yaml"), func(t *testing.T) {
			yaml, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			compiled, diagnostics := spec.CompileWith(yaml, spec.Options{Registry: reg})
			if compiled == nil {
				t.Fatalf("compile failed: %+v", diagnostics)
			}
			hash, err := compiled.Spec.Hash()
			if err != nil {
				t.Fatal(err)
			}
			warnings := []spec.Diagnostic{}
			for _, d := range diagnostics {
				if d.Severity == "warning" {
					warnings = append(warnings, d)
				}
			}
			got, err := json.MarshalIndent(golden{SpecHash: hash, Warnings: warnings, Spec: compiled.Spec}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			expectedPath := strings.TrimSuffix(source, ".yaml") + ".json"
			if *update {
				if err := os.WriteFile(expectedPath, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(expectedPath)
			if err != nil {
				t.Fatalf("read %s (run with -update to create it): %v", expectedPath, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("compiled spec differs from %s; run go test ./test/golden -update and review the diff\n got: %s", expectedPath, got)
			}
		})
	}
}

// TestRuleFixtures runs one failing fixture per validation rule (02 §6.2). The
// file name starts with the exact code; a leading "# also: CODE,CODE" comment
// lists other codes the fixture is allowed to produce.
func TestRuleFixtures(t *testing.T) {
	sources, err := filepath.Glob(filepath.Join("invalid", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reg := registry(t)
	seen := map[string]bool{}
	for _, source := range sources {
		want, allowed := fixtureCodes(t, source)
		seen[want] = true
		t.Run(filepath.Base(source), func(t *testing.T) {
			yaml, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			compiled, diagnostics := spec.CompileWith(yaml, spec.Options{Registry: reg})
			if compiled != nil {
				t.Fatalf("fixture compiled; want %s", want)
			}
			found := false
			for _, d := range diagnostics {
				if d.Severity != "error" {
					continue
				}
				if d.Code == want {
					found = true
				} else if !slices.Contains(allowed, d.Code) {
					t.Errorf("unexpected error %s: %s (%s)", d.Code, d.Message, d.Path)
				}
			}
			if !found {
				t.Fatalf("want %s, got %+v", want, diagnostics)
			}
		})
	}
	for _, code := range []string{
		"E_API_VERSION", "E_NAME_FORMAT", "E_NO_NODES", "E_DUP_NODE_ID", "E_NODE_ID_FORMAT",
		"E_UNKNOWN_NODE", "E_CYCLE", "E_NO_ROOT", "E_UNKNOWN_TYPE", "E_CONFIG",
		"E_EXPR_SYNTAX", "E_EXPR_SCOPE", "E_EXPR_TYPE", "E_SELF_REF", "E_BRANCH_TARGET",
		"E_RETRY_RANGE", "E_TIMEOUT_RANGE",
	} {
		if !seen[code] {
			t.Errorf("rule %s from 02 §6.2 has no failing fixture", code)
		}
	}
}

// TestWarningFixtures: each warning fixture publishes and carries its warning.
func TestWarningFixtures(t *testing.T) {
	sources, err := filepath.Glob(filepath.Join("warnings", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reg := registry(t)
	for _, source := range sources {
		want, _ := fixtureCodes(t, source)
		t.Run(filepath.Base(source), func(t *testing.T) {
			yaml, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			compiled, diagnostics := spec.CompileWith(yaml, spec.Options{Registry: reg})
			if compiled == nil {
				t.Fatalf("warning fixture did not compile: %+v", diagnostics)
			}
			if !slices.ContainsFunc(diagnostics, func(d spec.Diagnostic) bool { return d.Code == want && d.Severity == "warning" }) {
				t.Fatalf("want warning %s, got %+v", want, diagnostics)
			}
		})
	}
}

func fixtureCodes(t *testing.T, source string) (string, []string) {
	t.Helper()
	code, _, _ := strings.Cut(strings.TrimSuffix(filepath.Base(source), ".yaml"), "-")
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	var allowed []string
	if first, _, _ := strings.Cut(string(content), "\n"); strings.HasPrefix(first, "# also:") {
		for _, item := range strings.Split(strings.TrimPrefix(first, "# also:"), ",") {
			allowed = append(allowed, strings.TrimSpace(item))
		}
	}
	return code, allowed
}

// TestSpecHashStable compiles the same YAML 1 000 times and requires a single
// hash, then checks that reordering nodes and mapping keys does not change it.
func TestSpecHashStable(t *testing.T) {
	reg := registry(t)
	source, err := os.ReadFile("delivery.yaml")
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[string]int{}
	for range 1000 {
		compiled, diagnostics := spec.CompileWith(source, spec.Options{Registry: reg})
		if compiled == nil {
			t.Fatalf("compile: %+v", diagnostics)
		}
		hash, err := compiled.Spec.Hash()
		if err != nil {
			t.Fatal(err)
		}
		hashes[hash]++
	}
	if len(hashes) != 1 {
		t.Fatalf("1 000 compilations produced %d distinct hashes: %v", len(hashes), hashes)
	}

	a := `
apiVersion: workflow/v1
name: reorder
inputs:
  b: {type: string}
  a: {type: string, required: true}
nodes:
  - id: second
    type: transform
    needs: [first]
    config: {value: {y: '${input.b}', x: '${input.a}'}}
  - {id: first, type: noop, config: {}}
`
	b := `
apiVersion: workflow/v1
name: reorder
inputs:
  a: {required: true, type: string}
  b: {type: string}
nodes:
  - {id: first, type: noop, config: {}}
  - id: second
    needs: [first]
    type: transform
    config: {value: {x: '${input.a}', y: '${input.b}'}}
`
	hashOf := func(source string) string {
		compiled, diagnostics := spec.CompileWith([]byte(source), spec.Options{Registry: reg})
		if compiled == nil {
			t.Fatalf("compile: %+v", diagnostics)
		}
		hash, err := compiled.Spec.Hash()
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	if hashOf(a) != hashOf(b) {
		t.Fatal("equivalent YAML with different node and key order hashed differently")
	}
}
