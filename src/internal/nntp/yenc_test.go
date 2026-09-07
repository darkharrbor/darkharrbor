package nntp

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDecodeYEncLinesCRC(t *testing.T) {
	valid := []string{
		"=ybegin line=128 size=3 name=test.bin",
		"klm",
		"=yend size=3 pcrc32=a3830348",
	}

	decoded, err := DecodeYEncLines(valid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, []byte("ABC")) {
		t.Fatalf("decoded = %q, want ABC", decoded)
	}

	corrupt := append([]string(nil), valid...)
	corrupt[1] = "kln"
	if _, err := DecodeYEncLines(corrupt); err == nil || !strings.Contains(err.Error(), "crc32 mismatch") {
		t.Fatalf("corrupt article error = %v, want crc32 mismatch", err)
	}

	decoded, err = decodeYEncLines(corrupt, false)
	if err != nil {
		t.Fatalf("CRC-disabled decode: %v", err)
	}
	if !bytes.Equal(decoded, []byte("ABD")) {
		t.Fatalf("CRC-disabled decoded = %q, want ABD", decoded)
	}
}

func TestDecodeYEncReaderCapturesMultipartFileTotal(t *testing.T) {
	body := "=ybegin part=1 line=128 size=5000000000 name=test.bin\r\n" +
		"=ypart begin=1 end=3\r\n" +
		"klm\r\n" +
		"=yend size=3 pcrc32=a3830348\r\n"
	var total int64
	decoded, err := decodeYEncReaderWithTotal(context.Background(), strings.NewReader(body), true, &total)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, []byte("ABC")) {
		t.Fatalf("decoded = %q, want ABC", decoded)
	}
	if total != 5_000_000_000 {
		t.Fatalf("file total = %d, want 5000000000", total)
	}
}

func TestDecodeYEncLinesCRCCompatibility(t *testing.T) {
	tests := []struct {
		name string
		end  string
	}{
		{name: "whole file crc", end: "=yend size=3 crc32=a3830348"},
		{name: "missing crc", end: "=yend size=3"},
		{name: "malformed crc", end: "=yend size=3 pcrc32=invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoded, err := DecodeYEncLines([]string{
				"=ybegin line=128 size=3 name=test.bin",
				"klm",
				test.end,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decoded, []byte("ABC")) {
				t.Fatalf("decoded = %q, want ABC", decoded)
			}
		})
	}
}
