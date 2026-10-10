// Package problem writes RFC 7807 application/problem+json errors (04 §1).
package problem

import (
	"encoding/json"
	"fmt"
	"net/http"
)

const typeBase = "https://errors.workflow.internal/"

// Problem is an RFC 7807 body. Errors carries every diagnostic or field error,
// never only the first: a user with three typos should see three errors.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	Code     string `json:"code,omitempty"`
	Errors   any    `json:"errors,omitempty"`
}

func (p *Problem) Error() string { return p.Title + ": " + p.Detail }

// New builds a problem whose type URI and code come from slug.
func New(status int, slug, title, detail string) *Problem {
	return &Problem{Type: typeBase + slug, Title: title, Status: status, Detail: detail, Code: slug}
}

func BadRequest(detail string) *Problem {
	return New(http.StatusBadRequest, "bad_request", "Request is invalid", detail)
}

func NotFound(what string) *Problem {
	return New(http.StatusNotFound, "not_found", "Not found", fmt.Sprintf("%s was not found", what))
}

func Conflict(slug, detail string) *Problem {
	return New(http.StatusConflict, slug, "Conflict", detail)
}

func Unauthorized() *Problem {
	return New(http.StatusUnauthorized, "unauthorized", "Authentication required", "sign in or send an API key as Authorization: Bearer <key>")
}

func Forbidden(detail string) *Problem {
	return New(http.StatusForbidden, "forbidden", "Not allowed", detail)
}

func PaymentRequired(detail string) *Problem {
	return New(http.StatusPaymentRequired, "plan_limit", "Plan limit reached", detail)
}

func Internal() *Problem {
	return New(http.StatusInternalServerError, "internal", "Internal error", "the request could not be completed; it is safe to retry")
}

// Write sends p with instance set to the request path.
func Write(w http.ResponseWriter, r *http.Request, p *Problem) {
	if p.Instance == "" && r != nil {
		p.Instance = r.URL.Path
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
