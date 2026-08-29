package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"golang.org/x/sys/unix"
)

// routingReadBuffer is sized for one routing message. The kernel delivers
// these one per read, and the largest carries a handful of socket addresses.
const routingReadBuffer = 4096

// isNetworkChange reports whether a routing message means this machine's own
// network attachment changed — joined a network, lost one, moved to another.
//
// Only interface and address messages qualify. The route-table messages
// (RTM_ADD, RTM_DELETE, RTM_CHANGE) look like they should, and must not be
// used: the kernel clones a host route for ordinary outbound connections, so
// every `git fetch` this daemon runs produces an RTM_ADD of its own. Treating
// those as a reason to sync would make the daemon its own trigger — fetch,
// route added, fetch again — with nothing but the debounce between it and a
// spin. RTM_MISS and RTM_GET are likewise traffic, not configuration: the
// socket carries other processes' route lookups too.
//
// The cost of the narrow reading is a default route that changes with no
// address change, which goes unnoticed until the next interval tick. That is
// the right way round: a missed trigger delays one sync, a spurious one
// repeats forever.
func isNetworkChange(msgType byte) bool {
	switch msgType {
	case unix.RTM_NEWADDR, unix.RTM_DELADDR, unix.RTM_IFINFO, unix.RTM_IFINFO2:
		return true
	default:
		return false
	}
}

// routingMessageType reads the type out of a raw routing message. ok is false
// for a message too short to have a header, or one announcing a protocol
// version this build was not compiled against.
func routingMessageType(msg []byte) (msgType byte, ok bool) {
	// struct rt_msghdr opens with u_short rtm_msglen, u_char rtm_version,
	// u_char rtm_type — the only three fields every message type shares.
	if len(msg) < 4 {
		return 0, false
	}
	if msg[2] != unix.RTM_VERSION {
		return 0, false
	}
	return msg[3], true
}

// shutdownEventID identifies the user event that wakes the kqueue below on
// cancellation. Any value works; it only has to differ from the routing
// socket's descriptor, which a user event's identifier is not compared
// against.
const shutdownEventID = 1

// watchNetworkChanges calls onChange each time this machine's network
// attachment changes, until ctx is canceled.
//
// It reads the kernel's routing socket, which announces interface and address
// changes to anyone listening. That is a notification rather than a poll, so
// noticing that the network came back costs nothing while it is away — and
// the wait for it is a kqueue rather than a bare read for the same reason.
// Closing a routing socket does not interrupt a read already parked in it, so
// a read is only ever entered once kqueue says a message is there, and
// cancellation arrives as a user event on the same queue.
func watchNetworkChanges(ctx context.Context, onChange func(), logger *slog.Logger) error {
	sock, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return fmt.Errorf("opening the routing socket: %w", err)
	}
	defer unix.Close(sock)

	kq, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("creating the kqueue: %w", err)
	}
	defer unix.Close(kq)

	registrations := []unix.Kevent_t{
		{Ident: uint64(sock), Filter: unix.EVFILT_READ, Flags: unix.EV_ADD},
		{Ident: shutdownEventID, Filter: unix.EVFILT_USER, Flags: unix.EV_ADD | unix.EV_CLEAR},
	}
	if _, err := unix.Kevent(kq, registrations, nil, nil); err != nil {
		return fmt.Errorf("registering the routing socket with the kqueue: %w", err)
	}

	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
		case <-stopped:
			return
		}
		trigger := []unix.Kevent_t{{Ident: shutdownEventID, Filter: unix.EVFILT_USER, Fflags: unix.NOTE_TRIGGER}}
		if _, err := unix.Kevent(kq, trigger, nil, nil); err != nil {
			logger.Warn("waking the routing socket watcher failed", "error", err)
		}
	}()

	events := make([]unix.Kevent_t, 2)
	buf := make([]byte, routingReadBuffer)
	for {
		n, err := unix.Kevent(kq, nil, events, nil)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("waiting on the kqueue: %w", err)
		}
		for _, event := range events[:n] {
			if event.Filter == unix.EVFILT_USER {
				return nil
			}
			read, err := unix.Read(sock, buf)
			if err != nil {
				if err == unix.EINTR || err == unix.EAGAIN {
					continue
				}
				return fmt.Errorf("reading the routing socket: %w", err)
			}
			msgType, ok := routingMessageType(buf[:read])
			if !ok {
				continue
			}
			if isNetworkChange(msgType) {
				logger.Debug("routing socket reported a network change", "rtm_type", msgType)
				onChange()
			}
		}
	}
}
