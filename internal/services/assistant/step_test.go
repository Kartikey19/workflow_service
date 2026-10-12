package assistant

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kartikey/workflow_service/internal/spec"
)

func TestAuthoringContextExcludesNestedSchemaValuesAndRepairMessages(t *testing.T) {
	schema := []byte(`{"type":"object","description":"private-description","properties":{"text":{"type":"string","default":"private-default","enum":["private-enum"],"examples":["private-example"]},"items":{"type":"array","items":{"anyOf":[{"type":"string","default":"private-alternative"}]}}},"required":["text"]}`)
	raw, _ := json.Marshal(schemaStructure(schema))
	if strings.Contains(string(raw), "private-") {
		t.Fatal("schema values escaped authoring allowlist")
	}
	if !strings.Contains(string(raw), `"required":["text"]`) || !strings.Contains(string(raw), `"anyOf":[{"type":"string"}]`) {
		t.Fatal("authoring lost schema structure")
	}
	feedback := repairDiagnostics([]spec.Diagnostic{{Code: "invalid_config", Path: "nodes[1].config", Severity: "error", Message: "invalid value: private-value"}})
	if strings.Contains(feedback, "private-value") || !strings.Contains(feedback, "invalid_config") {
		t.Fatal("repair feedback must carry structure without values")
	}
}
