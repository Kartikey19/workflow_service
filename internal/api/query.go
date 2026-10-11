package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/internal/store/gen"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/uid"
	"net/http"
	"strconv"
	"time"
)

// runCursor is the opaque keyset cursor over (created_at, id) descending.
type runCursor struct {
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"id"`
}

func encodeCursor(run gen.Run) string {
	data, _ := json.Marshal(runCursor{CreatedAt: run.CreatedAt.Time, ID: uid.String(run.ID)})
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(text string) (pgtype.Timestamptz, pgtype.UUID, error) {
	data, err := base64.RawURLEncoding.DecodeString(text)
	var cursor runCursor
	if err == nil {
		err = json.Unmarshal(data, &cursor)
	}
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, exception.BadRequest("after is not a valid cursor")
	}
	id, err := uid.Parse(cursor.ID)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, exception.BadRequest("after is not a valid cursor")
	}
	return pgtype.Timestamptz{Time: cursor.CreatedAt, Valid: true}, id, nil
}

func queryLimit(r *http.Request, fallback, maximum int) (int, error) {
	text := r.URL.Query().Get("limit")
	if text == "" {
		return fallback, nil
	}
	limit, err := strconv.Atoi(text)
	if err != nil || limit < 1 || limit > maximum {
		return 0, exception.BadRequest(fmt.Sprintf("limit must be between 1 and %d", maximum))
	}
	return limit, nil
}

func optionalQueryTime(r *http.Request, name string) (pgtype.Timestamptz, error) {
	text := r.URL.Query().Get(name)
	if text == "" {
		return pgtype.Timestamptz{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return pgtype.Timestamptz{}, exception.BadRequest(name + " must be an RFC 3339 timestamp")
	}
	return pgtype.Timestamptz{Time: parsed, Valid: true}, nil
}

func optionalQueryText(r *http.Request, name string) pgtype.Text {
	text := r.URL.Query().Get(name)
	return pgtype.Text{String: text, Valid: text != ""}
}
