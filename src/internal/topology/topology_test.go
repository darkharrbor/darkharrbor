package topology

import "testing"

func TestProfilesMatchExactly(t *testing.T) {
	for _, name := range []Name{T1, T2, T3} {
		profile, ok := Profile(name)
		if !ok {
			t.Fatalf("Profile(%q) not recognized", name)
		}
		got, ok := Match(profile)
		if !ok || got != name {
			t.Fatalf("Match(Profile(%q)) = %q, %v", name, got, ok)
		}
	}

	unsupported := []Features{
		{AggregatedDiscovery: true},
		{UniversalProxy: true},
		{AggregatedDiscovery: true, UniversalProxy: true, Promotion: true},
		{ReactiveCommit: true, Promotion: true},
	}
	for _, features := range unsupported {
		if got, ok := Match(features); ok {
			t.Fatalf("unsupported combination matched %q", got)
		}
	}
}

func TestMissingCapabilities(t *testing.T) {
	if got := MissingCapabilities(T1, Features{}); len(got) != 0 {
		t.Fatalf("T1 missing capabilities = %v", got)
	}
	if got := MissingCapabilities(T2, Features{}); len(got) != 1 || got[0] != "universal-playback-proxy" {
		t.Fatalf("T2 missing capabilities = %v", got)
	}
	if got := MissingCapabilities(T3, Features{UniversalProxy: true}); len(got) != 2 {
		t.Fatalf("T3 missing capabilities = %v", got)
	}
}

func FuzzParse(f *testing.F) {
	f.Add("t1")
	f.Add(" T2 ")
	f.Add("unknown")
	f.Fuzz(func(t *testing.T, raw string) {
		name, ok := Parse(raw)
		if !ok {
			if name != T1 {
				t.Fatalf("invalid input fallback = %q", name)
			}
			return
		}
		profile, known := Profile(name)
		if !known {
			t.Fatalf("parsed unknown profile %q", name)
		}
		matched, exact := Match(profile)
		if !exact || matched != name {
			t.Fatalf("parsed profile does not round trip: %q -> %q", name, matched)
		}
	})
}
