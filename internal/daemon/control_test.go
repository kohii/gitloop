package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// drain reports which reasons are waiting in each trigger, so a test can
// assert on what dispatch actually fired.
func drain(triggers map[string]*repoTrigger) map[string]triggerReason {
	fired := map[string]triggerReason{}
	for path, trigger := range triggers {
		if reason := trigger.take(); reason != "" {
			fired[path] = reason
		}
	}
	return fired
}

func testTriggers(paths ...string) map[string]*repoTrigger {
	triggers := make(map[string]*repoTrigger, len(paths))
	for _, path := range paths {
		triggers[path] = newRepoTrigger()
	}
	return triggers
}

func TestControlServerDispatchesToNamedRepositories(t *testing.T) {
	triggers := testTriggers("/notes", "/journal")
	server := &controlServer{triggers: triggers, logger: discardLogger()}

	resp := server.dispatch(ControlRequest{Command: CommandSync, Paths: []string{"/notes"}})
	if resp.Error != "" {
		t.Fatalf("dispatch error = %q, want none", resp.Error)
	}
	if !slices.Equal(resp.Triggered, []string{"/notes"}) {
		t.Errorf("triggered = %v, want [/notes]", resp.Triggered)
	}

	fired := drain(triggers)
	if fired["/notes"] != TriggerManual {
		t.Errorf("/notes reason = %q, want %q", fired["/notes"], TriggerManual)
	}
	if _, ok := fired["/journal"]; ok {
		t.Error("/journal was triggered, but the request did not name it")
	}
}

func TestControlServerDispatchWithNoPathsMeansEveryRepository(t *testing.T) {
	triggers := testTriggers("/notes", "/journal")
	server := &controlServer{triggers: triggers, logger: discardLogger()}

	resp := server.dispatch(ControlRequest{Command: CommandSync})
	if resp.Error != "" {
		t.Fatalf("dispatch error = %q, want none", resp.Error)
	}
	if len(resp.Triggered) != 2 {
		t.Errorf("triggered = %v, want both repositories", resp.Triggered)
	}
	if fired := drain(triggers); len(fired) != 2 {
		t.Errorf("fired = %v, want both repositories", fired)
	}
}

// TestControlServerDispatchRefusesUnknownPathsWithoutFiringAny pins the
// all-or-nothing contract: naming three repositories and misspelling one must
// not half-run the request, because the caller has no way to tell from a
// partial success which half happened.
func TestControlServerDispatchRefusesUnknownPathsWithoutFiringAny(t *testing.T) {
	triggers := testTriggers("/notes", "/journal")
	server := &controlServer{triggers: triggers, logger: discardLogger()}

	resp := server.dispatch(ControlRequest{Command: CommandSync, Paths: []string{"/notes", "/typo"}})
	if resp.Error == "" {
		t.Fatal("dispatch accepted an unknown repository path, want an error")
	}
	if !strings.Contains(resp.Error, "/typo") {
		t.Errorf("error = %q, want it to name the unknown path", resp.Error)
	}
	if fired := drain(triggers); len(fired) != 0 {
		t.Errorf("fired = %v, want nothing: a refused request must change nothing", fired)
	}
}

func TestControlServerDispatchRejectsUnknownCommands(t *testing.T) {
	triggers := testTriggers("/notes")
	server := &controlServer{triggers: triggers, logger: discardLogger()}

	resp := server.dispatch(ControlRequest{Command: "rm -rf"})
	if resp.Error == "" {
		t.Fatal("dispatch accepted an unknown command, want an error")
	}
	if fired := drain(triggers); len(fired) != 0 {
		t.Errorf("fired = %v, want nothing", fired)
	}
}

func TestRequestSyncOverTheSocket(t *testing.T) {
	dir := shortStateDir(t)
	socketPath := filepath.Join(dir, controlSocketName)
	triggers := testTriggers("/notes")

	server, err := listenControl(socketPath, triggers, discardLogger())
	if err != nil {
		t.Fatalf("listenControl: %v", err)
	}
	done := make(chan struct{})
	go server.serve(done)
	defer close(done)

	resp, err := RequestSync(socketPath, []string{"/notes"})
	if err != nil {
		t.Fatalf("RequestSync: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("response error = %q, want none", resp.Error)
	}
	if !slices.Equal(resp.Triggered, []string{"/notes"}) {
		t.Errorf("triggered = %v, want [/notes]", resp.Triggered)
	}
	if fired := drain(triggers); fired["/notes"] != TriggerManual {
		t.Errorf("fired = %v, want /notes", fired)
	}
}

// TestListenControlRestrictsTheSocket pins the socket's permissions. Anyone
// who can connect can make the daemon commit and push, so the umask must not
// be what decides who that is.
func TestListenControlRestrictsTheSocket(t *testing.T) {
	socketPath := filepath.Join(shortStateDir(t), controlSocketName)
	server, err := listenControl(socketPath, testTriggers(), discardLogger())
	if err != nil {
		t.Fatalf("listenControl: %v", err)
	}
	defer server.listener.Close()

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
}

func TestListenControlReplacesASocketLeftByADeadDaemon(t *testing.T) {
	socketPath := filepath.Join(shortStateDir(t), controlSocketName)
	if err := os.WriteFile(socketPath, []byte("remains of a daemon that crashed"), 0o600); err != nil {
		t.Fatal(err)
	}

	server, err := listenControl(socketPath, testTriggers(), discardLogger())
	if err != nil {
		t.Fatalf("listenControl over a stale socket file: %v", err)
	}
	server.listener.Close()
}

// TestListenControlReportsAnOverlongPath covers the one failure the kernel
// describes uselessly: past sockaddr_un's sun_path it answers "invalid
// argument", which reads like a bug in the caller rather than a path that is
// simply too long.
func TestListenControlReportsAnOverlongPath(t *testing.T) {
	path := filepath.Join(shortStateDir(t), strings.Repeat("x", controlPathLimit), controlSocketName)

	_, err := listenControl(path, testTriggers(), discardLogger())
	if err == nil {
		t.Fatal("listenControl accepted a path longer than a unix socket allows")
	}
	if !strings.Contains(err.Error(), "unix socket allows") {
		t.Errorf("error = %q, want it to explain the length limit", err)
	}
}

func TestRequestSyncReportsNoDaemon(t *testing.T) {
	socketPath := filepath.Join(shortStateDir(t), controlSocketName)
	if _, err := RequestSync(socketPath, nil); err != ErrDaemonNotRunning {
		t.Errorf("RequestSync with nothing listening = %v, want ErrDaemonNotRunning", err)
	}
}

func TestAcquireProcessLockRefusesASecondDaemon(t *testing.T) {
	lockPath := filepath.Join(shortStateDir(t), processLockName)

	first, err := acquireProcessLock(lockPath)
	if err != nil {
		t.Fatalf("acquireProcessLock: %v", err)
	}

	if _, err := acquireProcessLock(lockPath); err != ErrDaemonAlreadyRunning {
		t.Errorf("second acquireProcessLock = %v, want ErrDaemonAlreadyRunning", err)
	}

	first.release()
	second, err := acquireProcessLock(lockPath)
	if err != nil {
		t.Fatalf("acquireProcessLock after release: %v", err)
	}
	second.release()
}
