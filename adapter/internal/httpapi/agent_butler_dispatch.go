package httpapi

import (
	"github.com/daboluocc/bbclaw/adapter/internal/butler"
)

// SetDispatchRecorder wires the process-level dispatch recorder into the
// server. Used alongside SetDispatchRing; both are recorded by butler.Engine.
func (s *Server) SetDispatchRecorder(r *butler.DispatchRecorder) {
	s.dispatchRecorder = r
}
