package wizard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

func TestConfigureHTTPBackendsAddsAndDialTestsSource(t *testing.T) {
	var out bytes.Buffer
	p := configurePrompter{
		reader: bufio.NewReaderSize(strings.NewReader("y\nn\nmedia\nstremio\nhttps://source.example\nhttps://client.example\nhttps://metadata.example\n"), 8*1024+1),
		out:    &out,
	}
	values := map[string]string{}
	secretUpdates := map[string]string{}
	var dialed config.HTTPBackend
	err := configureHTTPBackends(&p, &config.Config{}, values, secretUpdates, func(ctx context.Context, backend config.HTTPBackend) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("dial context is not bounded")
		}
		dialed = backend
		return nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if dialed.Name != "media" || dialed.Type != "stremio" || dialed.PublicURL != "https://client.example" ||
		len(dialed.RedirectOrigins) != 1 || dialed.RedirectOrigins[0] != "https://metadata.example" {
		t.Fatalf("dialed backend = %+v", dialed)
	}
	if values["HARRBOR_HTTP_BACKENDS"] != "media" || values["HARRBOR_HTTPBACKEND_MEDIA_TYPE"] != "stremio" {
		t.Fatalf("values = %#v", values)
	}
	if values["HARRBOR_HTTPBACKEND_MEDIA_PUBLIC_URL"] != "https://client.example" {
		t.Fatalf("public URL not recorded: %#v", values)
	}
	if values["HARRBOR_HTTPBACKEND_MEDIA_REDIRECT_ORIGINS"] != "https://metadata.example" {
		t.Fatalf("redirect origins not recorded: %#v", values)
	}
	if secretUpdates["HARRBOR_HTTPBACKEND_MEDIA_URL"] != "https://source.example" {
		t.Fatalf("backend URL was not staged for sealing: %#v", secretUpdates)
	}
	if _, ok := values["HARRBOR_HTTPBACKEND_MEDIA_URL"]; ok {
		t.Fatal("backend URL entered non-secret tuning")
	}
	if strings.Contains(out.String(), "https://source.example") {
		t.Fatal("wizard output leaked backend URL")
	}
	if !strings.Contains(out.String(), "names in search priority order") {
		t.Fatalf("HTTP ordering boundary missing from prompt:\n%s", out.String())
	}
}

func TestConfigureHTTPBackendsIADefaultsBlankURLToArchiveOrg(t *testing.T) {
	var out bytes.Buffer
	p := configurePrompter{
		reader: bufio.NewReaderSize(strings.NewReader("y\nn\nia\nia\n\n\n"), 8*1024+1),
		out:    &out,
	}
	values := map[string]string{}
	secretUpdates := map[string]string{}
	var dialed config.HTTPBackend
	err := configureHTTPBackends(&p, &config.Config{}, values, secretUpdates, func(ctx context.Context, backend config.HTTPBackend) error {
		dialed = backend
		return nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if dialed.Name != "ia" || dialed.Type != "ia" || dialed.URL != defaultIABaseURL {
		t.Fatalf("dialed backend = %+v, want URL %q", dialed, defaultIABaseURL)
	}
	if secretUpdates["HARRBOR_HTTPBACKEND_IA_URL"] != defaultIABaseURL {
		t.Fatalf("default IA URL was not staged for sealing: %#v", secretUpdates)
	}
	if _, ok := values["HARRBOR_HTTPBACKEND_IA_URL"]; ok {
		t.Fatal("backend URL entered non-secret tuning")
	}
	if !strings.Contains(out.String(), defaultIABaseURL) {
		t.Fatalf("default IA endpoint not disclosed to operator:\n%s", out.String())
	}
}

func TestConfigureHTTPBackendsIAExplicitURLOverridesDefault(t *testing.T) {
	var out bytes.Buffer
	p := configurePrompter{
		reader: bufio.NewReaderSize(strings.NewReader("y\nn\nia\nia\nhttps://mirror.example\n\n"), 8*1024+1),
		out:    &out,
	}
	values := map[string]string{}
	secretUpdates := map[string]string{}
	var dialed config.HTTPBackend
	err := configureHTTPBackends(&p, &config.Config{}, values, secretUpdates, func(ctx context.Context, backend config.HTTPBackend) error {
		dialed = backend
		return nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if dialed.URL != "https://mirror.example" {
		t.Fatalf("explicit IA URL was overridden by default: %+v", dialed)
	}
	if secretUpdates["HARRBOR_HTTPBACKEND_IA_URL"] != "https://mirror.example" {
		t.Fatalf("explicit IA URL was not staged for sealing: %#v", secretUpdates)
	}
}

func TestConfigureHTTPBackendsPreservesSealedURL(t *testing.T) {
	var cfg config.Config
	cfg.HTTPStream.Backends = []config.HTTPBackend{{
		Name: "media", Type: "stremio", URL: "https://sealed.example/private",
	}}
	var out bytes.Buffer
	p := configurePrompter{
		reader: bufio.NewReaderSize(strings.NewReader("y\n\n\n\n\n\n\n"), 8*1024+1),
		out:    &out,
	}
	values := map[string]string{}
	err := configureHTTPBackends(&p, &cfg, values, map[string]string{}, func(_ context.Context, backend config.HTTPBackend) error {
		if backend.URL != cfg.HTTPStream.Backends[0].URL {
			t.Fatal("sealed URL was not preserved internally")
		}
		return nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values["HARRBOR_HTTPBACKEND_MEDIA_URL"]; ok {
		t.Fatal("sealed URL was copied to non-secret tuning")
	}
	if strings.Contains(out.String(), cfg.HTTPStream.Backends[0].URL) {
		t.Fatal("sealed URL leaked to output")
	}
}

func TestConfigureGenericSourceNeedsOnlyPrivateDescriptorFile(t *testing.T) {
	var out bytes.Buffer
	p := configurePrompter{
		reader: bufio.NewReaderSize(strings.NewReader("y\nn\ncloud\ngeneric\n/config/cloud-descriptors.json\n\n"), 8*1024+1),
		out:    &out,
	}
	values := map[string]string{}
	secretUpdates := map[string]string{}
	var dialed config.HTTPBackend
	err := configureHTTPBackends(&p, &config.Config{}, values, secretUpdates, func(_ context.Context, backend config.HTTPBackend) error {
		dialed = backend
		return nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if dialed.URL != "" || dialed.Type != "generic" || dialed.DescriptorsFile != "/config/cloud-descriptors.json" {
		t.Fatalf("dialed backend=%+v", dialed)
	}
	if len(secretUpdates) != 0 {
		t.Fatalf("generic source staged an unused URL secret: %#v", secretUpdates)
	}
	if !strings.Contains(out.String(), "no unused backend URL") {
		t.Fatalf("generic contract was not explained:\n%s", out.String())
	}
}

func TestConfigureHTTPBackendsDialFailureFailsClosedAndRedacts(t *testing.T) {
	var out bytes.Buffer
	p := configurePrompter{
		reader: bufio.NewReaderSize(strings.NewReader("y\nn\nmedia\nia\nhttps://source.example\n\n"), 8*1024+1),
		out:    &out,
	}
	err := configureHTTPBackends(&p, &config.Config{}, map[string]string{}, map[string]string{}, func(context.Context, config.HTTPBackend) error {
		return errors.New("request to https://private.example/path?token=hidden failed")
	}, false)
	if err == nil {
		t.Fatal("dial failure accepted")
	}
	if strings.Contains(err.Error(), "private.example") || strings.Contains(err.Error(), "hidden") {
		t.Fatalf("dial failure leaked sensitive URL: %v", err)
	}
}

func TestConfigureHTTPBackendsApprovesSanitizedRedirectOriginAndRetries(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/manifest.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":"org.example","version":"1.2.3","resources":[{"name":"stream","types":["movie"],"idPrefixes":["tt"]}]}`))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path+"?token=must-not-appear", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	var out bytes.Buffer
	input := "y\ny\nmedia\nstremio\n" + source.URL + "\n\n\ny\n"
	p := configurePrompter{reader: bufio.NewReaderSize(strings.NewReader(input), 8*1024+1), out: &out}
	values := map[string]string{}
	secrets := map[string]string{}
	if err := configureHTTPBackends(&p, &config.Config{}, values, secrets, nil, false); err != nil {
		t.Fatal(err)
	}
	if values["HARRBOR_HTTPBACKEND_MEDIA_REDIRECT_ORIGINS"] != target.URL {
		t.Fatalf("approved origin not saved: %#v", values)
	}
	if strings.Contains(out.String(), "must-not-appear") || strings.Contains(out.String(), "/manifest.json") {
		t.Fatalf("wizard exposed complete redirect URL: %s", out.String())
	}
	if !strings.Contains(out.String(), target.URL) {
		t.Fatalf("wizard did not show exact approval origin: %s", out.String())
	}
	if secrets["HARRBOR_HTTPBACKEND_MEDIA_URL"] != source.URL {
		t.Fatal("backend URL was not kept in sealed updates")
	}
	if _, ok := httpstream.RedirectOrigin(errors.New("ordinary failure")); ok {
		t.Fatal("ordinary error produced redirect approval signal")
	}
}

func TestConfigureHTTPBackendBounds(t *testing.T) {
	var names []string
	for i := 0; i < maxConfigureBackends+1; i++ {
		names = append(names, "s"+strings.Repeat("x", i))
	}
	if _, err := parseConfigureBackends(strings.Join(names, ",")); err == nil {
		t.Fatal("over-cap backend list accepted")
	}
	p := configurePrompter{
		reader: bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 8*1024+2)+"\n"), 8*1024+1),
		out:    &bytes.Buffer{},
	}
	if _, err := p.text("bounded", ""); err == nil {
		t.Fatal("oversize input accepted")
	}
}

func TestInitialHTTPConfigurationJoinsRedactedReviewBeforeMutation(t *testing.T) {
	withResponses(t, []string{
		"n", "media", "stremio", "https://secret.example/user/token", "", "",
	})
	var out bytes.Buffer
	values, secrets, err := collectInitialHTTPConfiguration(&out, func(context.Context, config.HTTPBackend) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if values["HARRBOR_HTTP_BACKENDS"] != "media" {
		t.Fatalf("values=%#v", values)
	}
	if secrets["HARRBOR_HTTPBACKEND_MEDIA_URL"] != "https://secret.example/user/token" {
		t.Fatalf("secret URL was not staged for sealing: %#v", secrets)
	}
	printInitialHTTPConfiguration(&out, values, secrets)
	if strings.Contains(out.String(), "secret.example") || !strings.Contains(out.String(), "[hidden; will be sealed]") {
		t.Fatalf("first-run HTTP review leaked or omitted redaction:\n%s", out.String())
	}
}

func FuzzParseConfigureBackends(f *testing.F) {
	f.Add("media,archive")
	f.Add("none")
	f.Fuzz(func(t *testing.T, raw string) {
		names, err := parseConfigureBackends(raw)
		if err != nil {
			return
		}
		if len(names) > maxConfigureBackends {
			t.Fatalf("accepted %d backends", len(names))
		}
		for _, name := range names {
			if name == "" || len(name) > 64 {
				t.Fatalf("accepted invalid name length")
			}
		}
	})
}

func TestParseConfigureBackendsPreservesPriorityOrder(t *testing.T) {
	names, err := parseConfigureBackends("stremio,omss,archive")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != "stremio,omss,archive" {
		t.Fatalf("priority order changed: %s", got)
	}
}
