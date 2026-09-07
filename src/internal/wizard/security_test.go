package wizard

import "testing"

func TestWildcardListenAddress(t *testing.T) {
	for _, address := range []string{":8381", "0.0.0.0:8381", "[::]:8381"} {
		if !wildcardListenAddress(address) {
			t.Errorf("%q was not recognized as wildcard", address)
		}
	}
	for _, address := range []string{"127.0.0.1:8381", "[::1]:8381", "localhost:8381", "malformed"} {
		if wildcardListenAddress(address) {
			t.Errorf("%q was recognized as wildcard", address)
		}
	}
}
