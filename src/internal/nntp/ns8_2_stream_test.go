package nntp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"sync"
	"testing"
)

func encodeYEncTestBody(data []byte, lineSize int) string {
	if lineSize < 1 {
		lineSize = 128
	}
	var body strings.Builder
	fmt.Fprintf(&body, "=ybegin line=%d size=%d name=test.bin\r\n", lineSize, len(data))
	lineLen := 0
	for _, b := range data {
		encoded := b + 42
		unit := []byte{encoded}
		switch encoded {
		case 0, '\t', '\n', '\r', ' ', '.', '=':
			unit = []byte{'=', encoded + 64}
		}
		if lineLen > 0 && lineLen+len(unit) > lineSize {
			body.WriteString("\r\n")
			lineLen = 0
		}
		body.Write(unit)
		lineLen += len(unit)
	}
	body.WriteString("\r\n")
	fmt.Fprintf(&body, "=yend size=%d pcrc32=%08x\r\n", len(data), crc32.ChecksumIEEE(data))
	return body.String()
}

func TestDecodeYEncReaderEscapesMultipart(t *testing.T) {
	want := []byte{0, 4, 19, 214, 224, 227, 255, 'A', 'B', 'C'}
	body := strings.Replace(
		encodeYEncTestBody(want, 4),
		"\r\n",
		"\r\n=ypart begin=1 end=10\r\n",
		1,
	)
	got, err := decodeYEncReader(context.Background(), strings.NewReader(body), true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded = %v, want %v", got, want)
	}
}

func TestDecodeYEncReaderFailsClosed(t *testing.T) {
	tests := map[string]string{
		"empty":            "",
		"missing begin":    "klm\r\n=yend size=3\r\n",
		"missing end":      "=ybegin line=128 size=3 name=x\r\nklm\r\n",
		"truncated escape": "=ybegin line=128 size=1 name=x\r\n=\r\n=yend size=1\r\n",
		"data after end":   "=ybegin line=128 size=3 name=x\r\nklm\r\n=yend size=3\r\nextra\r\n",
		"duplicate begin":  "=ybegin line=128 size=3 name=x\r\n=ybegin line=128 size=3 name=x\r\nklm\r\n=yend size=3\r\n",
		"wrong end size":   "=ybegin line=128 size=3 name=x\r\nklm\r\n=yend size=2\r\n",
		"malformed size":   "=ybegin line=128 size=3 name=x\r\nklm\r\n=yend size=nope\r\n",
		"oversized line":   "=ybegin line=128 size=1 name=x\r\n" + strings.Repeat("k", yEncMaxLineBytes+1) + "\r\n=yend size=1\r\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := decodeYEncReader(context.Background(), strings.NewReader(body), false)
			if err == nil || got != nil {
				t.Fatalf("decoded=%d bytes error=%v, want nil and error", len(got), err)
			}
		})
	}
}

func TestDecodeYEncReaderCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := decodeYEncReader(ctx, strings.NewReader(encodeYEncTestBody([]byte("cancel"), 128)), false)
	if got != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, errYEncUndrained) {
		t.Fatalf("decoded=%v error=%v, want cancelled undrained failure", got, err)
	}
}

func TestDecodeYEncReaderConcurrent(t *testing.T) {
	body := encodeYEncTestBody(bytes.Repeat([]byte("streaming"), 8192), 128)
	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := decodeYEncReader(context.Background(), strings.NewReader(body), true)
			if err == nil && len(got) != 9*8192 {
				err = fmt.Errorf("decoded length = %d", len(got))
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestBodyDecodedDotUnstuffsAndReusesConnection(t *testing.T) {
	ln, host, port := listenNS81(t)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := bufio.NewWriter(conn)
		if _, err = w.WriteString("200 ready\r\n"); err == nil {
			err = w.Flush()
		}
		for i := 0; err == nil && i < 2; i++ {
			if _, err = r.ReadString('\n'); err != nil {
				break
			}
			if i == 0 {
				_, err = fmt.Fprintf(w, "222 body\r\n=ybegin line=128 size=1 name=x\r\n..\r\n=yend size=1 pcrc32=%08x\r\n.\r\n", crc32.ChecksumIEEE([]byte{4}))
			} else {
				_, err = w.WriteString("222 body\r\n=ybegin line=128 size=3 name=x\r\nklm\r\n=yend size=3 pcrc32=a3830348\r\n.\r\n")
			}
			if err == nil {
				err = w.Flush()
			}
		}
		serverErr <- err
	}()

	pool := NewPool(PoolConfig{Host: host, Port: port, MaxConns: 1, PipelineDepth: 1}, nil)
	t.Cleanup(pool.Close)
	for i, want := range [][]byte{{4}, []byte("ABC")} {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got, err := conn.BodyDecoded(context.Background(), fmt.Sprintf("article-%d", i), true)
		pool.Release(conn, err)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("request %d decoded=%v error=%v, want %v", i, got, err, want)
		}
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func legacyDecodeYEncBody(body string) ([]byte, error) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	result := make([]byte, 0, len(lines)*128)
	var end string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "=ybegin"), strings.HasPrefix(line, "=ypart"), line == "":
			continue
		case strings.HasPrefix(line, "=yend"):
			end = line
		default:
			result = append(result, decodeYEncLine(line)...)
		}
	}
	if err := validateYEnd(end, result); err != nil {
		return nil, err
	}
	return result, nil
}

func BenchmarkYEncDecode(b *testing.B) {
	body := encodeYEncTestBody(bytes.Repeat([]byte("allocation-evidence-"), 1<<16), 128)
	b.Run("legacy-lines", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := legacyDecodeYEncBody(body); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("streaming-pooled", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := decodeYEncReader(context.Background(), strings.NewReader(body), true); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func FuzzDecodeYEncReader(f *testing.F) {
	f.Add([]byte("=ybegin line=128 size=3 name=test.bin\r\nklm\r\n=yend size=3 pcrc32=a3830348\r\n"))
	f.Add([]byte("=ybegin line=128 size=1 name=x\r\n=\r\n"))
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 64<<10 {
			t.Skip()
		}
		first, firstErr := decodeYEncReader(context.Background(), bytes.NewReader(body), false)
		second, secondErr := decodeYEncReader(context.Background(), bytes.NewReader(body), false)
		if (firstErr == nil) != (secondErr == nil) || !bytes.Equal(first, second) {
			t.Fatalf("non-deterministic decode: first=%d/%v second=%d/%v", len(first), firstErr, len(second), secondErr)
		}
	})
}
