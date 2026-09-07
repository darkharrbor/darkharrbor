// doctor_handler.go implements ONB-02's GET /api/v1/doctor endpoint: the
// same read-only sweep as `darkharrbor doctor`, but governed through the
// real running server's own accountgov.Governor (TorrentCDNGovOp,
// accountgov.PriorityAudit) instead of running ungoverned, since this
// process actually has one -- the same "govern through the real production
// lease when one is reachable" precedent debugresolve.go (OBS-02)
// established.
package api

import (
	"net/http"

	"github.com/darkharrbor/darkharrbor/internal/doctor"
)

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}

	opts := doctor.Options{
		DB:               s.store.DB(),
		Cfg:              s.cfg,
		DockerProxyURL:   s.doctorDockerProxyURL,
		PublicIPResolver: s.publicIPResolver,
	}
	if s.torrentGov != nil {
		opts.TorrentGov = s.torrentGov
		opts.TorrentGovOp = TorrentCDNGovOp
	}

	timeout := doctor.TimeoutFromEnv(func(msg string) { s.log.Warn("doctor: " + msg) })
	report, err := doctor.Run(r.Context(), opts, timeout)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	status := http.StatusOK
	if !report.OK() {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, report)
}
