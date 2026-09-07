package config

import "testing"

func TestDefaultServerAddressIsLoopback(t *testing.T) {
	if got := defaultConfig().Server.Address; got != "127.0.0.1:8381" {
		t.Fatalf("default server address = %q", got)
	}
}
