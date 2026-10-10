package spec

import "testing"

func TestValidateAcceptsForwardReferencesAndDefaults(t *testing.T) {
	doc, err := Decode([]byte(`
apiVersion: workflow/v1
name: example-flow
defaults:
  timeout: 15s
nodes:
  - id: second
    type: noop
    needs: [first]
    config: {}
  - id: first
    type: noop
    config: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	ApplyDefaults(doc)
	if got := Validate(doc); len(got) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", got)
	}
}

func TestValidateCollectsErrorsWithSourcePaths(t *testing.T) {
	doc, err := Decode([]byte(`
apiVersion: workflow/v2
name: INVALID
nodes:
  - id: task
    type: noop
    needs: [missing, missing]
    join: invalid
    retry:
      max_attempts: 0
      initial: 2s
      max: 1s
  - id: task
    type: sleep
    needs: [missing]
    config: {}
    timeout: 800h
    on_timeout: absent
`))
	if err != nil {
		t.Fatal(err)
	}
	ApplyDefaults(doc)
	got := Validate(doc)
	expected := map[string]string{
		"apiVersion": "E_API_VERSION", "name": "E_NAME_FORMAT",
		"nodes[0].config": "E_CONFIG", "nodes[0].join": "E_JOIN",
		"nodes[0].retry.max_attempts": "E_RETRY_RANGE",
		"nodes[0].retry.initial":      "E_RETRY_RANGE",
		"nodes[1].id":                 "E_DUP_NODE_ID", "nodes[1].timeout": "E_TIMEOUT_RANGE",
		"nodes": "E_NO_ROOT", "nodes[0].needs[0]": "E_UNKNOWN_NODE",
		"nodes[1].on_timeout": "E_UNKNOWN_NODE",
	}
	for path, code := range expected {
		found := false
		for _, diagnostic := range got {
			if diagnostic.Path == path && diagnostic.Code == code && diagnostic.Severity == "error" {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s at %s; got %+v", code, path, got)
		}
	}
}

func TestValidateReportsMissingHTTPTimeoutAsWarning(t *testing.T) {
	doc := &Document{APIVersion: "workflow/v1", Name: "example-flow", Nodes: []Node{{ID: "request", Type: "http", Config: map[string]any{}}}}
	ApplyDefaults(doc)
	got := Validate(doc)
	if len(got) != 1 || got[0].Code != "W_NO_TIMEOUT" || got[0].Severity != "warning" {
		t.Fatalf("diagnostics = %+v", got)
	}
}
