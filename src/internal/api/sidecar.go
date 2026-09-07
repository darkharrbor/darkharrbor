package api

import (
	"bytes"
	"context"
	"errors"
	"mime"
	"net/http"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
	"github.com/darkharrbor/darkharrbor/internal/sidecar"
)

func (s *Server) registerSidecars(mux *http.ServeMux) {
	mux.HandleFunc("/sidecar/{resource_id}", s.handleSidecarResource)
}

// CreateSidecarResource is the shared registration surface consumed by
// lane/player adapters in their own rows.
func (s *Server) CreateSidecarResource(ctx context.Context, in sidecar.Input) (sidecar.Resource, error) {
	if s.sidecars == nil {
		return sidecar.Resource{}, errors.New("sidecar: registry not wired")
	}
	return s.sidecars.Register(ctx, in)
}

func (s *Server) handleSidecarResource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.sidecars == nil {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	resource, err := s.sidecars.Resolve(r.Context(), r.PathValue("resource_id"))
	if err != nil {
		if errors.Is(err, sidecar.ErrNotFound) {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		s.log.Warn("sidecar: resolve failed", "error", httpstream.Sanitize(err))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	serveSidecarContent(w, r, resource)
}

func serveSidecarContent(w http.ResponseWriter, r *http.Request, resource sidecar.Resource) {
	w.Header().Set("Content-Type", resource.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": resource.Filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	http.ServeContent(w, r, resource.Filename, resource.CreatedAt, bytes.NewReader(resource.Bytes))
}
