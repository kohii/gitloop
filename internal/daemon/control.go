package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// controlSocketName is the daemon's request socket, kept next to the
	// status file in gitloop's state directory.
	controlSocketName = "control.sock"
	// controlRequestLimit bounds how much a client can send before being cut
	// off. A sync request is a short list of paths; anything larger is a
	// mistake or an attempt to make the daemon allocate.
	controlRequestLimit = 64 << 10
	// controlExchangeTimeout bounds one request/response round trip, so a
	// client that connects and then goes quiet cannot pin a goroutine.
	controlExchangeTimeout = 5 * time.Second
	// controlPathLimit is how long a unix socket path may be on macOS, from
	// sockaddr_un's 104-byte sun_path and the terminator it needs. Past it the
	// kernel answers "invalid argument", which says nothing about the length.
	controlPathLimit = 103
)

// CommandSync is the only control command: run a sync cycle now.
const CommandSync = "sync"

// ControlRequest is what a client sends, as one line of JSON.
type ControlRequest struct {
	Command string `json:"command"`
	// Paths selects repositories by their configured path. Empty means every
	// configured repository.
	Paths []string `json:"paths,omitempty"`
}

// ControlResponse is what the daemon sends back, as one line of JSON.
type ControlResponse struct {
	// Triggered lists the repositories a cycle was requested for.
	Triggered []string `json:"triggered,omitempty"`
	// Error explains a refused request. It is set instead of Triggered, never
	// alongside it: a request naming several repositories is accepted or
	// refused as a whole, so a typo in one path cannot half-run.
	Error string `json:"error,omitempty"`
}

// ControlSocketPath returns the socket a running daemon listens on, derived
// from the same state directory as the status file.
func ControlSocketPath() (string, error) {
	statusPath, err := DefaultStatusPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(statusPath), controlSocketName), nil
}

// ErrDaemonNotRunning is returned by RequestSync when nothing is listening on
// the control socket.
var ErrDaemonNotRunning = errors.New("no gitloop daemon is listening")

// RequestSync asks the daemon listening at socketPath to run a cycle now for
// paths (or for every configured repository, if paths is empty).
func RequestSync(socketPath string, paths []string) (ControlResponse, error) {
	conn, err := net.DialTimeout("unix", socketPath, controlExchangeTimeout)
	if err != nil {
		// A missing socket file and a socket left behind by a dead daemon are
		// the same thing to a user: nothing is listening.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return ControlResponse{}, ErrDaemonNotRunning
		}
		return ControlResponse{}, err
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(controlExchangeTimeout)); err != nil {
		return ControlResponse{}, err
	}
	if err := json.NewEncoder(conn).Encode(ControlRequest{Command: CommandSync, Paths: paths}); err != nil {
		return ControlResponse{}, err
	}

	var resp ControlResponse
	if err := json.NewDecoder(io.LimitReader(conn, controlRequestLimit)).Decode(&resp); err != nil {
		return ControlResponse{}, fmt.Errorf("reading the daemon's reply: %w", err)
	}
	return resp, nil
}

// controlServer answers control requests for one daemon's set of
// repositories.
type controlServer struct {
	listener net.Listener
	triggers map[string]*repoTrigger
	logger   *slog.Logger
}

// listenControl binds the control socket at path.
//
// A socket file already there is removed first. That is only safe because the
// caller holds the daemon's process lock: it proves no other daemon is
// listening, so the file can only be the remains of one that died.
func listenControl(path string, triggers map[string]*repoTrigger, logger *slog.Logger) (*controlServer, error) {
	if len(path) > controlPathLimit {
		return nil, fmt.Errorf("the control socket path %s is %d bytes, over the %d a unix socket allows", path, len(path), controlPathLimit)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the control socket directory: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing the stale control socket: %w", err)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	// Bound by the umask otherwise, which commonly leaves the socket group-
	// and world-connectable. Anyone who can connect can make this daemon
	// commit and push.
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("restricting the control socket: %w", err)
	}

	return &controlServer{listener: listener, triggers: triggers, logger: logger}, nil
}

// serve accepts and answers requests until ctx is canceled, at which point
// the listener is closed and the socket file removed.
func (s *controlServer) serve(done <-chan struct{}) {
	go func() {
		<-done
		// Unblocks the Accept below; the listener also unlinks the socket
		// file on close.
		s.listener.Close()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-done:
				return
			default:
			}
			// An accept that fails for any other reason is not fatal on its
			// own — the next one may well succeed — but a listener that has
			// genuinely broken would spin here, so give up on it.
			s.logger.Error("control socket stopped accepting connections", "error", err)
			return
		}
		go s.handle(conn)
	}
}

func (s *controlServer) handle(conn net.Conn) {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(controlExchangeTimeout)); err != nil {
		return
	}

	var req ControlRequest
	if err := json.NewDecoder(io.LimitReader(conn, controlRequestLimit)).Decode(&req); err != nil {
		s.reply(conn, ControlResponse{Error: fmt.Sprintf("could not read the request: %v", err)})
		return
	}
	s.reply(conn, s.dispatch(req))
}

// dispatch resolves a request against the configured repositories and fires
// the triggers it names. Every path is checked before any trigger is fired,
// so a request either runs in full or changes nothing.
func (s *controlServer) dispatch(req ControlRequest) ControlResponse {
	if req.Command != CommandSync {
		return ControlResponse{Error: fmt.Sprintf("unknown command %q", req.Command)}
	}

	targets := req.Paths
	if len(targets) == 0 {
		targets = make([]string, 0, len(s.triggers))
		for path := range s.triggers {
			targets = append(targets, path)
		}
	}

	var unknown []string
	for _, path := range targets {
		if _, ok := s.triggers[path]; !ok {
			unknown = append(unknown, path)
		}
	}
	if len(unknown) > 0 {
		return ControlResponse{Error: fmt.Sprintf("not a configured repository: %v", unknown)}
	}

	for _, path := range targets {
		s.triggers[path].fire(TriggerManual)
	}
	s.logger.Info("sync requested over the control socket", "repositories", len(targets))
	return ControlResponse{Triggered: targets}
}

func (s *controlServer) reply(conn net.Conn, resp ControlResponse) {
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		s.logger.Warn("writing a control socket reply failed", "error", err)
	}
}
