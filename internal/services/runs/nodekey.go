package runs

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/pkg/uid"
)

// NodeKey is stable across attempts and lease re-deliveries of one node, and
// differs between runs (01 §5). Never derive it from a node_executions.id.
func NodeKey(runID pgtype.UUID, nodeID, branchKey string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s", uid.String(runID), nodeID, branchKey))
	return "wf_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:15])
}
