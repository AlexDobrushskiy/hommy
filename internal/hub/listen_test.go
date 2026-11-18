package hub

import (
	"slices"
	"testing"
)

// The hub Mac's own host, pair, doctor and send dial HUB_URL (127.0.0.1 by
// default): a HUB_LISTEN on one other address keeps a loopback listener too.
func TestListenAddrsKeepLoopback(t *testing.T) {
	for in, want := range map[string][]string{
		"127.0.0.1:8470":  {"127.0.0.1:8470"},
		"localhost:8470":  {"localhost:8470"},
		"0.0.0.0:8470":    {"0.0.0.0:8470"},
		":8470":           {":8470"},
		"[::]:8470":       {"[::]:8470"},
		"192.0.2.10:8470": {"192.0.2.10:8470", "127.0.0.1:8470"},
		"[::1]:8470":      {"[::1]:8470", "127.0.0.1:8470"},
	} {
		if got := listenAddrs(in); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", in, got, want)
		}
	}
}
