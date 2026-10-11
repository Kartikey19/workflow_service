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
	r.mux.Handle(pattern, r.server.authed(role, false, h))
}

// HandlePublic registers pattern without authentication. It still bounds the
// body and records request metrics.
func (r router) HandlePublic(pattern string, h func(http.ResponseWriter, *http.Request)) {
	r.mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		started := time.Now()
		recorder := httpx.NewStatusRecorder(w)
		defer func() {
			obs.HTTPRequests.WithLabelValues(req.Pattern, strconv.Itoa(recorder.Status)).Observe(time.Since(started).Seconds())
		}()
		req.Body = http.MaxBytesReader(recorder, req.Body, maxBodyBytes)
		h(recorder, req)
	}))
}

// HandleAdmin registers pattern for platform administrators only; tenant
// roles, including owner, are not enough.
func (r router) HandleAdmin(pattern string, h api.HandlerFunc) {
	r.mux.Handle(pattern, r.server.authed(auth.RoleViewer, true, h))
}

// authed resolves the principal, checks the minimum role, bounds the body and
// records request metrics.
func (s *Server) authed(role string, platformAdmin bool, h api.HandlerFunc) http.Handler {
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
		if platformAdmin && !principal.PlatformAdmin {
			exception.Write(recorder, r, exception.Forbidden("this action needs the platform-admin flag"))
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
