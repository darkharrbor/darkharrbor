package arrsetup

import (
	"context"
	"strings"
)

// TopologyReport is a read-only snapshot of whether one arr instance has
// exactly the 3-indexer/2-client DH topology Register (HR0.3) creates,
// using the exact same identity rules Register itself uses (implementation
// + baseUrl for indexers; implementation + host + port for download
// clients). It never creates, updates, or deletes anything.
type TopologyReport struct {
	Name              string
	QBitPresent       bool
	SABPresent        bool
	TorznabPresent    bool
	NewznabPresent    bool
	HTTPStreamPresent bool
	// Err is set only for a transport/auth/decode failure talking to the
	// arr; in that case every *Present field is meaningless and should be
	// ignored by the caller (abstain, not a false "absent").
	Err error
}

// Complete reports whether all five expected DH entries are present. Only
// meaningful when Err is nil.
func (r TopologyReport) Complete() bool {
	return r.QBitPresent && r.SABPresent && r.TorznabPresent && r.NewznabPresent && r.HTTPStreamPresent
}

// CheckTopology performs a read-only GET of one arr's indexers and download
// clients and reports which of the 5 expected DH entries are present, using
// Register's own identity rules. A transport/auth/decode failure abstains
// (Err set, all *Present fields meaningless) rather than reporting a false
// "absent".
func CheckTopology(ctx context.Context, t ArrTarget) TopologyReport {
	r := TopologyReport{Name: t.Name}
	c := &arrClient{baseURL: strings.TrimRight(t.URL, "/"), apiKey: t.APIKey, hc: newHTTPClient()}

	clients, err := listDownloadClients(ctx, c)
	if err != nil {
		r.Err = err
		return r
	}
	for _, cl := range clients {
		host := fieldStr(cl.Fields, "host")
		port := fieldInt(cl.Fields, "port")
		if host != dhHost || port != dhPort {
			continue
		}
		switch cl.Implementation {
		case "QBittorrent":
			r.QBitPresent = true
		case "Sabnzbd":
			r.SABPresent = true
		}
	}

	indexers, err := listIndexers(ctx, c)
	if err != nil {
		r.Err = err
		return r
	}
	for _, idx := range indexers {
		if idx.Implementation != "Torznab" && idx.Implementation != "Newznab" {
			continue
		}
		base := fieldStr(idx.Fields, "baseUrl")
		switch {
		case idx.Implementation == "Torznab" && base == torznabBase:
			r.TorznabPresent = true
		case idx.Implementation == "Newznab" && base == newznabBase:
			r.NewznabPresent = true
		case idx.Implementation == "Torznab" && base == httpStreamBase:
			r.HTTPStreamPresent = true
		}
	}
	return r
}
