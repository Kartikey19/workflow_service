package server

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/kartikey/workflow_service/internal/api"
	"github.com/kartikey/workflow_service/pkg/auth"
	"github.com/kartikey/workflow_service/pkg/exception"
	"github.com/kartikey/workflow_service/pkg/httpx"
	"github.com/kartikey/workflow_service/pkg/obs"
)

// maxBodyBytes bounds request bodies: workflow YAML, run input, signal payloads.
const maxBodyBytes = 1 << 20

// router registers handlers on the mux behind authentication and a minimum role.
type router struct {
	server *Server
	mux    *http.ServeMux
}

var _ api.Router = router{}

// Handle registers pattern for principals with at least role.
func (r router) Handle(pattern, role string, h api.HandlerFunc) {
	r.mux.Handle(pattern, r.server.authed(role, h))
}

// authed resolves the principal, checks the minimum role, bounds the body and
// records request metrics.
func (s *Server) authed(role string, h api.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := httpx.NewStatusRecorder(w)
		defer func() {
			obs.HTTPRequests.WithLabelValues(r.Pattern, strconv.Itoa(recorder.Status)).Observe(time.Since(started).Seconds())
		}()
		principal, err := s.services.Auth.Authenticate(r)
		if err != nil {
			exception.Write(recorder, r, exception.Unauthorized())
			return
		}
		if !principal.Can(role) {
			exception.Write(recorder, r, exception.Forbidden(fmt.Sprintf("this action needs the %s role or higher", role)))
			return
		}
		r.Body = http.MaxBytesReader(recorder, r.Body, maxBodyBytes)
		h(recorder, r.WithContext(auth.WithPrincipal(r.Context(), principal)), principal)
	})
}
