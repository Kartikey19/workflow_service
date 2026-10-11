// Package uid generates and formats the UUID identifiers used across the service.
package uid

import (
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
