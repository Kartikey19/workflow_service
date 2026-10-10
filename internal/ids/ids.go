// Package ids generates the identifiers shared by the API and the engine.
package ids

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5/pgtype"
)

// New returns a time-ordered UUIDv7 in pgx form.
func New() pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte(uuid.NewV7()), Valid: true}
}

// String formats a pgx UUID; an invalid UUID formats as the empty string.
func String(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuid.UUID(id.Bytes).String()
}

// Parse accepts the canonical textual UUID form.
func Parse(text string) (pgtype.UUID, error) {
	parsed, err := uuid.Parse(text)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid UUID")
	}
	return pgtype.UUID{Bytes: [16]byte(parsed), Valid: true}, nil
}

// NodeKey is stable across attempts and lease re-deliveries of one node, and
// differs between runs (01 §5). Never derive it from a node_executions.id.
func NodeKey(runID pgtype.UUID, nodeID, branchKey string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s", String(runID), nodeID, branchKey))
	return "wf_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:15])
}
