package wizard

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

// RunConfigureExistingAIOStreams connects an existing saved AIOStreams user
// without taking ownership of AIOStreams or StremThru.
func RunConfigureExistingAIOStreams(opts ConfigureOptions) (bool, error) {
	if opts.Current == nil {
		return false, fmt.Errorf("current configuration is required")
	}
	if opts.Path == "" {
		opts.Path = config.DefaultTuningPath
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	p := configurePrompter{reader: bufio.NewReaderSize(opts.Stdin, 8*1024+1), out: opts.Stdout}
	values, err := config.ReadTuning(opts.Path)
	if err != nil {
		return false, err
	}
	originalValues := cloneStringMap(values)

	fmt.Fprintln(p.out, "\nConnect existing AIOStreams")
	fmt.Fprintln(p.out, "AIOStreams and StremThru remain externally managed; this writes only DarkHarrbor settings.")
	internalOrigin, err := p.text("AIOStreams origin reachable from DarkHarrbor", "http://aiostreams:3000")
	if err != nil {
		return false, err
	}
	directManifest, entered, err := p.secret("AIOStreams Direct Manifest URL", false)
	if err != nil {
		return false, err
	}
	if !entered {
		return false, fmt.Errorf("direct Manifest URL is required; no changes made")
	}
	backend, err := existingAIOStreamsBackend(internalOrigin, directManifest)
	if err != nil {
		return false, err
	}

	mediaFlowDefault := strings.TrimSpace(opts.Current.MediaFlow.BaseURL)
	if mediaFlowDefault == "" {
		mediaFlowDefault = strings.TrimSpace(opts.Current.Stremio.ClientBaseURL)
	}
	mediaFlowOrigin, err := p.text("MediaFlow client origin (restricted listener)", mediaFlowDefault)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(mediaFlowOrigin) == "" {
		return false, fmt.Errorf("MediaFlow client origin is required for aggregator playback")
	}

	names := make([]string, 0, len(opts.Current.HTTPStream.Backends)+1)
	seenAIOStreams := false
	for _, current := range opts.Current.HTTPStream.Backends {
		names = append(names, current.Name)
		if current.Name == backend.Name {
			seenAIOStreams = true
		}
	}
	if !seenAIOStreams {
		names = append(names, backend.Name)
	}
	values["HARRBOR_HTTP_STREAM_ENABLED"] = "true"
	values["HARRBOR_HTTP_BACKENDS"] = strings.Join(names, ",")
	values["HARRBOR_HTTPBACKEND_AIOSTREAMS_TYPE"] = "stremio"
	values["HARRBOR_HTTPBACKEND_AIOSTREAMS_PUBLIC_URL"] = backend.PublicURL
	values["HARRBOR_MEDIAFLOW_BASE_URL"] = strings.TrimSpace(mediaFlowOrigin)
	delete(values, "HARRBOR_HTTPBACKEND_AIOSTREAMS_URL")
	if err := config.ValidateTuning(values); err != nil {
		return false, err
	}

	dial := opts.DialBackend
	if dial == nil {
		dial = func(ctx context.Context, candidate config.HTTPBackend) error {
			return dialHTTPBackend(ctx, opts.Current, candidate)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), configureDialTimeout)
	err = dial(ctx, backend)
	cancel()
	if err != nil {
		return false, fmt.Errorf("AIOStreams saved-user dial test failed: %s", httpstream.Sanitize(err))
	}
	fmt.Fprintln(p.out, "  AIOStreams saved-user dial test passed")
	confirmed, err := p.yesNo("Save existing AIOStreams integration", true)
	if err != nil || !confirmed {
		if err == nil {
			fmt.Fprintln(p.out, "No changes made.")
		}
		return false, err
	}
	secretUpdates := map[string]string{"HARRBOR_HTTPBACKEND_AIOSTREAMS_URL": backend.URL}
	if err := applyConfigureFiles(opts.Path, opts.SecretsPath, opts.Current.Secrets, values, originalValues, secretUpdates); err != nil {
		return false, err
	}
	fmt.Fprintf(p.out, "Saved %s\n", opts.Path)
	fmt.Fprintln(p.out, "Restart DarkHarrbor, then run: darkharrbor doctor")
	return true, nil
}

func existingAIOStreamsBackend(internalOrigin, directManifest string) (config.HTTPBackend, error) {
	internal, err := url.Parse(strings.TrimSpace(internalOrigin))
	if err != nil || (internal.Scheme != "http" && internal.Scheme != "https") || internal.Host == "" ||
		internal.User != nil || internal.RawQuery != "" || internal.Fragment != "" ||
		(internal.Path != "" && internal.Path != "/") {
		return config.HTTPBackend{}, fmt.Errorf("AIOStreams internal origin must be an http(s) origin without credentials, path, query, or fragment")
	}
	manifest, err := url.Parse(strings.TrimSpace(directManifest))
	if err != nil || (manifest.Scheme != "http" && manifest.Scheme != "https") || manifest.Host == "" ||
		manifest.User != nil || manifest.RawQuery != "" || manifest.Fragment != "" {
		return config.HTTPBackend{}, fmt.Errorf("direct Manifest URL must be an absolute http(s) URL without userinfo, query, or fragment")
	}
	if !strings.HasPrefix(manifest.Path, "/stremio/") || !strings.HasSuffix(manifest.Path, "/manifest.json") {
		return config.HTTPBackend{}, fmt.Errorf("direct Manifest URL must use /stremio/.../manifest.json")
	}
	savedUserPath := strings.TrimSuffix(manifest.EscapedPath(), "/manifest.json")
	internal.Path = ""
	internal.RawPath = ""
	internal.RawQuery = ""
	internal.Fragment = ""
	return config.HTTPBackend{
		Name:      "aiostreams",
		Type:      "stremio",
		URL:       strings.TrimRight(internal.String(), "/") + savedUserPath,
		PublicURL: manifest.Scheme + "://" + manifest.Host,
	}, nil
}
