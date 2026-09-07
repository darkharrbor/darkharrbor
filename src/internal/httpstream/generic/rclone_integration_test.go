package generic

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/httpstream"
)

func TestRcloneWebDAVSearchResolveRangeReorderAndRestart(t *testing.T) {
	rclone, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone is not installed")
	}
	ip := privateFixtureIP(t)
	listener, err := net.Listen("tcp4", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("private fixture address unavailable: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	root := t.TempDir()
	collection := filepath.Join(root, "library")
	if err := os.Mkdir(collection, 0o700); err != nil {
		t.Fatal(err)
	}
	alpha := fixtureMedia(32 << 10)
	if err := os.WriteFile(filepath.Join(collection, "alpha.mkv"), alpha, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(collection, "beta.mkv"), fixtureMedia(16<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.mkv"), fixtureMedia(8<<10), 0o600); err != nil {
		t.Fatal(err)
	}

	stop := startRcloneWebDAV(t, rclone, root, addr)
	defer func() { stop() }()
	baseURL := "http://" + addr + "/library/"
	descriptorDir := t.TempDir()
	if err := os.Chmod(descriptorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	descriptors := filepath.Join(descriptorDir, "descriptors.json")
	body := fmt.Sprintf(`[{"source_id":"rclone-fixture","kind":"webdav","title":"Rclone Fixture","year":2026,"url":%q}]`, baseURL)
	if err := os.WriteFile(descriptors, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	metadataClient := httpstream.NewHTTPClient(httpstream.NewSecurityPolicy(false), httpstream.TrustBackend, httpstream.TransportMetadata)
	active := New("cloud", descriptors, metadataClient)
	results, err := active.Search(context.Background(), httpstream.StreamQuery{Title: "Rclone Fixture", Year: 2026})
	if err != nil || len(results) != 1 {
		t.Fatalf("search results=%d err=%v", len(results), err)
	}
	key := results[0].Key
	if strings.ContainsAny(key.Selector, "/?") {
		t.Fatalf("selector contains source coordinates: %q", key.Selector)
	}
	resolveSelected := func() (httpstream.ResolvedFile, error) {
		files, resolveErr := active.Resolve(context.Background(), httpstream.ResolveRequest{Key: key})
		if resolveErr != nil {
			return httpstream.ResolvedFile{}, resolveErr
		}
		return httpstream.MatchSelector(files, key.Selector)
	}
	selected, err := resolveSelected()
	if err != nil || selected.Name != "alpha.mkv" || selected.Size != int64(len(alpha)) {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}

	streamClient := httpstream.NewHTTPClient(httpstream.NewSecurityPolicy(true), httpstream.TrustSource, httpstream.TransportStream)
	source, err := httpstream.NewByteSource(httpstream.ByteSourceConfig{
		Key: "rclone-fixture-alpha", Size: int64(len(alpha)), RangeVerified: true, Client: streamClient,
		Resolve: func(context.Context, bool) (httpstream.ResolvedFile, error) {
			file, resolveErr := resolveSelected()
			file.SupportsRange = true
			return file, resolveErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 777)
	if n, readErr := source.ReadAt(context.Background(), buf, 4097); readErr != nil || n != len(buf) || !bytes.Equal(buf, alpha[4097:4097+len(buf)]) {
		t.Fatalf("ranged read n=%d err=%v", n, readErr)
	}

	if err := os.WriteFile(filepath.Join(collection, "000-new.mkv"), fixtureMedia(4<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	if afterInsert, resolveErr := resolveSelected(); resolveErr != nil || afterInsert.Name != "alpha.mkv" {
		t.Fatalf("listing insertion rebound selection: file=%+v err=%v", afterInsert, resolveErr)
	}

	stop()
	stop = startRcloneWebDAV(t, rclone, root, addr)
	active = New("cloud", descriptors, metadataClient)
	if afterRestart, resolveErr := resolveSelected(); resolveErr != nil || afterRestart.Name != "alpha.mkv" {
		t.Fatalf("restart rebound selection: file=%+v err=%v", afterRestart, resolveErr)
	}
}

func privateFixtureIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate fixture addresses: %v", err)
	}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err == nil && ip.To4() != nil && !ip.IsLoopback() && ip.IsPrivate() {
			return ip.String()
		}
	}
	t.Skip("no non-loopback private IPv4 address for TrustSource fixture")
	return ""
}

func startRcloneWebDAV(t *testing.T, binary, root, addr string) func() {
	t.Helper()
	var logs bytes.Buffer
	cmd := exec.Command(binary, "serve", "webdav", root, "--addr", addr, "--read-only", "--log-level", "ERROR")
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start rclone WebDAV: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp4", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("rclone WebDAV did not listen: %s", httpstream.ScrubURLs(logs.String()))
		}
		time.Sleep(25 * time.Millisecond)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		})
	}
}

func fixtureMedia(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i*17 + i/97) % 251)
	}
	return data
}
