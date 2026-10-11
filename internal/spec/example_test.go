package spec

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/kartikey/workflow_service/internal/core/expr"
)

func TestSupportTicketExample(t *testing.T) {
	source, err := os.ReadFile("../../examples/support-ticket.yaml")
	if err != nil {
		t.Fatal(err)
	}
	compiled, diagnostics := Compile(source)
	if compiled == nil || hasErrors(diagnostics) {
		t.Fatalf("compile: %+v", diagnostics)
	}
	if !reflect.DeepEqual(compiled.Spec.TopoOrder, []string{"create_ticket", "alert_oncall"}) {
		t.Fatalf("unexpected order: %v", compiled.Spec.TopoOrder)
	}
	for _, priority := range []string{"normal", "high"} {
		t.Run(priority, func(t *testing.T) {
			bindings := expr.Bindings{
				Input: map[string]any{"request_id": "REQ-1001", "customer_email": "customer@example.com", "subject": "Payment failed", "priority": priority},
				Nodes: map[string]expr.NodeView{
					"create_ticket": {Status: "succeeded", Output: map[string]any{"body": map[string]any{"ticket_id": "TKT-42"}}},
				},
			}
			alert := compiled.Programs.Nodes["alert_oncall"]
			ok, err := alert.If.EvalBool(context.Background(), bindings)
			if err != nil || ok != (priority == "high") {
				t.Fatalf("alert condition: %v, %v", ok, err)
			}
			config, err := alert.Config.Resolve(context.Background(), bindings)
			if err != nil {
				t.Fatal(err)
			}
			body := config.(map[string]any)["body"].(map[string]any)
			if body["ticket_id"] != "TKT-42" {
				t.Fatalf("alert body: %v", body)
			}
			output, err := compiled.Programs.Outputs.Resolve(context.Background(), bindings)
			if err != nil || !reflect.DeepEqual(output, map[string]any{"ticket_id": "TKT-42"}) {
				t.Fatalf("output: %v, %v", output, err)
			}
		})
	}
}
