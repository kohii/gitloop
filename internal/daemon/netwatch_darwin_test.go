package daemon

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestIsNetworkChangeIgnoresRouteTableTraffic is the test this filter exists
// for. The routing socket carries far more than configuration changes: the
// kernel clones a host route for ordinary outbound connections, so gitloop's
// own `git fetch` produces an RTM_ADD, and other processes' route lookups
// arrive as RTM_GET and RTM_MISS. Treating any of those as a reason to sync
// would make the daemon its own trigger.
func TestIsNetworkChangeIgnoresRouteTableTraffic(t *testing.T) {
	cases := map[string]struct {
		msgType byte
		want    bool
	}{
		"address added to an interface":     {unix.RTM_NEWADDR, true},
		"address removed from an interface": {unix.RTM_DELADDR, true},
		"interface state changed":           {unix.RTM_IFINFO, true},
		"interface state changed (v2)":      {unix.RTM_IFINFO2, true},

		"route added by an outbound connection": {unix.RTM_ADD, false},
		"route deleted":                         {unix.RTM_DELETE, false},
		"route changed":                         {unix.RTM_CHANGE, false},
		"another process looked a route up":     {unix.RTM_GET, false},
		"a route lookup failed":                 {unix.RTM_MISS, false},
		"an address is being resolved":          {unix.RTM_RESOLVE, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isNetworkChange(tc.msgType); got != tc.want {
				t.Errorf("isNetworkChange(%d) = %v, want %v", tc.msgType, got, tc.want)
			}
		})
	}
}

func TestRoutingMessageType(t *testing.T) {
	cases := []struct {
		name   string
		msg    []byte
		want   byte
		wantOK bool
	}{
		{
			name:   "a well-formed header",
			msg:    []byte{0x14, 0x00, unix.RTM_VERSION, unix.RTM_NEWADDR, 0xff},
			want:   unix.RTM_NEWADDR,
			wantOK: true,
		},
		{
			name:   "too short to have a header",
			msg:    []byte{0x14, 0x00, unix.RTM_VERSION},
			wantOK: false,
		},
		{
			name:   "a protocol version this build does not know",
			msg:    []byte{0x14, 0x00, unix.RTM_VERSION + 1, unix.RTM_NEWADDR},
			wantOK: false,
		},
		{
			name:   "empty",
			msg:    nil,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := routingMessageType(tc.msg)
			if ok != tc.wantOK {
				t.Fatalf("routingMessageType() ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("routingMessageType() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestWatchNetworkChangesShutsDownCleanly covers the part a unit test can't:
// that the routing socket opens at all, and that canceling the context
// unblocks the read it is parked in.
func TestWatchNetworkChangesShutsDownCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- watchNetworkChanges(ctx, func() {}, discardLogger()) }()

	// Give the goroutine time to reach its read before pulling the rug out,
	// so this exercises the close-unblocks-read path rather than an early
	// return.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("watchNetworkChanges returned %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchNetworkChanges did not return within 2s of context cancellation")
	}
}
