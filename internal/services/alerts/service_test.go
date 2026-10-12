package alerts

import (
	"strings"
	"testing"

	"github.com/kartikey/workflow_service/internal/services/runs"
	"github.com/kartikey/workflow_service/pkg/uid"
)

func TestCheckURLRedactsSlackCredential(t *testing.T) {
	target, err := checkURL(Slack, "https://hooks.slack.com/services/T000/B000/abcdefSECRETwxyz")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(target, "SECRET") || !strings.HasPrefix(target, "hooks.slack.com/") {
		t.Fatalf("target %q exposes the Slack credential", target)
	}
	for _, bad := range []string{"", "ftp://example.com/x", "https://user:pass@example.com/", "/relative"} {
		if _, err := checkURL(Webhook, bad); err == nil {
			t.Errorf("checkURL accepted %q", bad)
		}
	}
}

func TestCheckEventsAndRecipients(t *testing.T) {
	if events, err := checkEvents(nil); err != nil || len(events) != len(Events) {
		t.Fatalf("default events = %v, %v", events, err)
	}
	if _, err := checkEvents([]string{"run.exploded"}); err == nil {
		t.Fatal("accepted an unknown event")
	}
	if events, _ := checkEvents([]string{runs.AlertRunFailed, runs.AlertRunFailed}); len(events) != 1 {
		t.Fatalf("duplicates kept: %v", events)
	}
	if _, err := checkRecipients([]string{"ops@example.com", " "}); err != nil {
		t.Fatal(err)
	}
	if _, err := checkRecipients([]string{"not an address"}); err == nil {
		t.Fatal("accepted an invalid address")
	}
	if _, err := checkRecipients(nil); err == nil {
		t.Fatal("accepted no recipients")
	}
}

func TestRenderLinksToRun(t *testing.T) {
	s := &Service{publicURL: "https://weft.example.com"}
	m := s.render("d1", runs.AlertRunFailed, uid.New(), map[string]any{
		"workflow": "triage", "run_id": "r-1", "node_id": "classify", "message": "model unavailable",
	})
	if m.subject != "Workflow triage is failing" {
		t.Fatalf("subject %q", m.subject)
	}
	if !strings.Contains(m.text, "Step classify failed: model unavailable") || !strings.Contains(m.text, "https://weft.example.com/ui/runs/r-1") {
		t.Fatalf("text %q", m.text)
	}
	recovered := s.render("d2", runs.AlertRunRecovered, uid.New(), map[string]any{"workflow": "triage", "failures": 3})
	if !strings.Contains(recovered.text, "after 3 failed") {
		t.Fatalf("recovery text %q", recovered.text)
	}
}
