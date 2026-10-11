// Package httpx holds small HTTP helpers shared by handlers and middleware.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kartikey/workflow_service/pkg/exception"
)

// WriteJSON sends value as an uncacheable JSON response.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// DecodeJSON reads one JSON object, rejecting unknown fields and trailing data.
func DecodeJSON(r *http.Request, into any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return err
		}
		if errors.Is(err, io.EOF) {
			return exception.BadRequest("request body is required")
		}
		return exception.BadRequest("invalid JSON: " + err.Error())
	}
	if decoder.More() {
		return exception.BadRequest("request body must contain one JSON object")
	}
	return nil
}

// Time is RFC 3339 UTC with millisecond precision (04 §1); null when unset.
func Time(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	formatted := t.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	return &formatted
}

// Text is a nullable string column; null when unset.
func Text(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

// Raw passes stored JSONB through; SQL NULL becomes JSON null.
func Raw(data []byte) json.RawMessage {
	if len(data) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(data)
}
