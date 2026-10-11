//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var yamlFence = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// TestWorkflowLint is the CI workflow lint: every shipped workflow must be
// accepted by POST /be/v1/workflows?dry_run=true, the same path an editor or a
// pipeline uses. Run it alone with `make lint-workflows`.
func TestWorkflowLint(t *testing.T) {
	h := newHarness(t, t.Context())
	c := apiClient{t: t, base: h.apiServer(nil), tenant: h.tenant}
	lint := func(name, source string) {
		t.Run(name, func(t *testing.T) {
			status, body := c.yaml("/be/v1/workflows?dry_run=true", source)
			if status != 200 || body["valid"] != true {
				t.Fatalf("dry run = %d %v", status, body)
			}
		})
	}

	root := filepath.Join("..", "..")
	var files []string
	for _, pattern := range []string{"examples/*.yaml", "test/golden/*.yaml"} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found to lint")
	}
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lint(strings.TrimPrefix(file, root+string(filepath.Separator)), string(source))
	}

	// Every complete example in the spec document (02 §5) must stay publishable.
	// A block that uses a node type this build does not register yet (agent,
	// human_task) is skipped until that executor exists.
	_, executors := c.json("GET", "/be/v1/executors", nil)
	registered := map[string]bool{}
	for _, item := range executors["items"].([]any) {
		registered[item.(map[string]any)["type"].(string)] = true
	}
	doc, err := os.ReadFile(filepath.Join(root, "docs", "02-workflow-spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	section := string(doc)
	start := strings.Index(section, "## 5. Complete examples")
	end := strings.Index(section, "## 6.")
	if start < 0 || end < start {
		t.Fatal("complete examples section not found in docs/02-workflow-spec.md")
	}
	blocks := yamlFence.FindAllStringSubmatch(section[start:end], -1)
	if len(blocks) < 3 {
		t.Fatalf("found %d complete examples, want 3", len(blocks))
	}
	for _, block := range blocks {
		name := "docs/02-workflow-spec.md#" + regexp.MustCompile(`(?m)^name: (\S+)`).FindStringSubmatch(block[1])[1]
		missing := ""
		for _, m := range regexp.MustCompile(`(?m)^\s*- id: \S+\n\s+type: (\w+)`).FindAllStringSubmatch(block[1], -1) {
			if !registered[m[1]] {
				missing = m[1]
			}
		}
		if missing != "" {
			t.Run(name, func(t *testing.T) { t.Skipf("node type %q is not registered yet", missing) })
			continue
		}
		lint(name, block[1])
	}
}
