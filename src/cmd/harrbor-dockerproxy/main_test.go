package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkharrbor/darkharrbor/internal/dockerpolicy"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := f(r)
	if resp != nil && resp.Request == nil {
		resp.Request = r
	}
	return resp, err
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func requestBody(user string, cmd []string) *bytes.Reader {
	body, _ := json.Marshal(execCreateRequest{Cmd: cmd, User: user, AttachStdout: true, AttachStderr: true})
	return bytes.NewReader(body)
}

func TestHandlerExecPolicyAndIssuedID(t *testing.T) {
	var creates int
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/sonarr/json"):
			return response(200, `{"Name":"/sonarr","Config":{"Image":"lscr.io/linuxserver/sonarr:latest"},"State":{"Running":true}}`), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/sonarr/exec"):
			creates++
			return response(201, `{"Id":"proxy-issued"}`), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec/proxy-issued/start"):
			var framed bytes.Buffer
			header := make([]byte, 8)
			header[0] = 1
			binary.BigEndian.PutUint32(header[4:], 3)
			framed.Write(header)
			framed.WriteString("ok\n")
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(&framed)}, nil
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/exec/proxy-issued/json"):
			return response(200, `{"ExitCode":0,"Running":false}`), nil
		default:
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	handler := newHandler(transport, parseTargets("sonarr"))

	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, httptest.NewRequest(http.MethodPost, "/v1.51/containers/sonarr/exec", requestBody("root", []string{"id"})))
	if denied.Code != http.StatusForbidden || creates != 0 {
		t.Fatalf("arbitrary command: status=%d creates=%d", denied.Code, creates)
	}

	created := httptest.NewRecorder()
	handler.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/v1.51/containers/sonarr/exec", requestBody("", dockerpolicy.ResolveFFprobe())))
	if created.Code != http.StatusCreated || creates != 1 {
		t.Fatalf("approved create: status=%d creates=%d body=%s", created.Code, creates, created.Body.String())
	}

	detached := httptest.NewRecorder()
	handler.ServeHTTP(detached, httptest.NewRequest(http.MethodPost, "/v1.51/exec/proxy-issued/start", strings.NewReader(`{"Detach":true,"Tty":false}`)))
	if detached.Code != http.StatusForbidden {
		t.Fatalf("detached start status=%d", detached.Code)
	}

	forged := httptest.NewRecorder()
	handler.ServeHTTP(forged, httptest.NewRequest(http.MethodPost, "/v1.51/exec/forged/start", strings.NewReader(`{"Detach":false,"Tty":false}`)))
	if forged.Code != http.StatusForbidden {
		t.Fatalf("forged exec ID status=%d", forged.Code)
	}

	started := httptest.NewRecorder()
	handler.ServeHTTP(started, httptest.NewRequest(http.MethodPost, "/v1.51/exec/proxy-issued/start", strings.NewReader(`{"Detach":false,"Tty":false}`)))
	if started.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", started.Code, started.Body.String())
	}

	replayed := httptest.NewRecorder()
	handler.ServeHTTP(replayed, httptest.NewRequest(http.MethodPost, "/v1.51/exec/proxy-issued/start", strings.NewReader(`{"Detach":false,"Tty":false}`)))
	if replayed.Code != http.StatusForbidden {
		t.Fatalf("replayed start status=%d", replayed.Code)
	}

	inspected := httptest.NewRecorder()
	handler.ServeHTTP(inspected, httptest.NewRequest(http.MethodGet, "/v1.51/exec/proxy-issued/json", nil))
	if inspected.Code != http.StatusOK {
		t.Fatalf("inspect status=%d body=%s", inspected.Code, inspected.Body.String())
	}
}

func TestHandlerRejectsWrongImageAndUnknownExecFields(t *testing.T) {
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return response(200, `{"Name":"/db","Config":{"Image":"postgres:latest"},"State":{"Running":true}}`), nil
	})
	handler := newHandler(transport, parseTargets("db"))

	wrongImage := httptest.NewRecorder()
	handler.ServeHTTP(wrongImage, httptest.NewRequest(http.MethodPost, "/v1.51/containers/db/exec", requestBody("", dockerpolicy.ReadArrConfig())))
	if wrongImage.Code != http.StatusForbidden {
		t.Fatalf("wrong image status=%d", wrongImage.Code)
	}

	unknownField := httptest.NewRecorder()
	body := `{"Cmd":["id"],"AttachStdout":true,"AttachStderr":true,"Env":["PATH=/evil"]}`
	handler.ServeHTTP(unknownField, httptest.NewRequest(http.MethodPost, "/v1.51/containers/db/exec", strings.NewReader(body)))
	if unknownField.Code != http.StatusForbidden {
		t.Fatalf("unknown field status=%d", unknownField.Code)
	}
}

func TestDecodeExecStartRequiresBoundedAttachedShape(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"attached", `{"Detach":false,"Tty":false}`, true},
		{"detached", `{"Detach":true,"Tty":false}`, false},
		{"tty", `{"Detach":false,"Tty":true}`, false},
		{"unknown field", `{"Detach":false,"Tty":false,"ConsoleSize":[80,24]}`, false},
		{"trailing json", `{"Detach":false,"Tty":false}{}`, false},
		{"oversize", `{"Detach":false,"Tty":false,"Padding":"` + strings.Repeat("x", 2048) + `"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1.51/exec/id/start", strings.NewReader(tt.body))
			_, got := decodeExecStart(recorder, request)
			if got != tt.want {
				t.Fatalf("decodeExecStart()=%v, want %v; status=%d", got, tt.want, recorder.Code)
			}
		})
	}
}

func TestHandlerRequiresExplicitTargetApproval(t *testing.T) {
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return response(200, `{"Name":"/sonarr","Config":{"Image":"lscr.io/linuxserver/sonarr:latest"},"State":{"Running":true}}`), nil
	})
	handler := newHandler(transport, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1.51/containers/sonarr/exec", requestBody("", dockerpolicy.ReadArrConfig())))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("unapproved target status=%d", recorder.Code)
	}
}

func TestProxyRewritePreservesPathAndDropsForwardingHeaders(t *testing.T) {
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "http" || r.URL.Host != "docker" || r.URL.Path != "/v1.51/containers/json" {
			t.Fatalf("rewritten URL = %s", r.URL.String())
		}
		for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
			if r.Header.Get(name) != "" {
				t.Fatalf("untrusted %s header reached Docker", name)
			}
		}
		return response(http.StatusOK, `[]`), nil
	})
	handler := newHandler(transport, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1.51/containers/json", nil)
	req.Header.Set("Forwarded", "for=attacker")
	req.Header.Set("X-Forwarded-For", "attacker")
	req.Header.Set("X-Forwarded-Host", "attacker")
	req.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerSanitizesContainerMetadataAndDeniesInspect(t *testing.T) {
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return response(200, `[{"Id":"abc","Names":["/sonarr"],"Image":"lscr.io/linuxserver/sonarr:latest","State":"running","Labels":{"secret":"do-not-return"},"Mounts":[{"Source":"/private"}]}]`), nil
	})
	handler := newHandler(transport, nil)

	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, "/v1.51/containers/json", nil))
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), "secret") || strings.Contains(listed.Body.String(), "private") {
		t.Fatalf("unsanitized list response: status=%d body=%s", listed.Code, listed.Body.String())
	}

	inspect := httptest.NewRecorder()
	handler.ServeHTTP(inspect, httptest.NewRequest(http.MethodGet, "/v1.51/containers/abc/json", nil))
	if inspect.Code != http.StatusForbidden {
		t.Fatalf("container inspect status=%d", inspect.Code)
	}
}

func TestHandlerSanitizesUnversionedDockerVersion(t *testing.T) {
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/version" {
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		return response(http.StatusOK, `{"ApiVersion":"1.47","Version":"27.5.1","GitCommit":"private-noise","KernelVersion":"private-noise"}`), nil
	})
	handler := newHandler(transport, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/version", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("version status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.TrimSpace(recorder.Body.String()) != `{"ApiVersion":"1.47"}` {
		t.Fatalf("unsanitized version response: %s", recorder.Body.String())
	}
}

func TestSanitizeContainerListRejectsOversizeResponse(t *testing.T) {
	resp := response(200, strings.Repeat(" ", maxContainerListResponse+1))
	if err := sanitizeContainerList(resp); err == nil || !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("oversized list error=%v", err)
	}
}

func TestSanitizeEventsDropsExtraAttributes(t *testing.T) {
	body := `{"Type":"container","Action":"start","Actor":{"ID":"abc","Attributes":{"image":"sonarr","name":"sonarr","secret":"drop-me"}},"time":1}`
	req := httptest.NewRequest(http.MethodGet, "/v1.51/events", nil)
	resp := response(200, body)
	resp.Request = req
	if err := sanitizeEvents(resp); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "secret") || strings.Contains(string(out), "drop-me") {
		t.Fatalf("event attributes were not sanitized: %s", out)
	}
	if !strings.Contains(string(out), `"image":"sonarr"`) || !strings.Contains(string(out), `"name":"sonarr"`) {
		t.Fatalf("required event attributes missing: %s", out)
	}
}
