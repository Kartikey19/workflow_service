package httpx

import "net/http"

// StatusRecorder captures the response status for metrics and keeps Flush
// available for server-sent events.
type StatusRecorder struct {
	http.ResponseWriter
	Status int
}

// NewStatusRecorder wraps w; the status defaults to 200 like net/http.
func NewStatusRecorder(w http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: w, Status: http.StatusOK}
}

func (r *StatusRecorder) WriteHeader(status int) {
	r.Status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *StatusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *StatusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
