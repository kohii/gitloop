package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Phase values for RepoStatus.Phase.
const (
	// PhaseIdle means the repository isn't mid-cycle and has no known
	// unresolved conflict; it's safe for another process to save into it.
	PhaseIdle = "idle"
	// PhaseSyncing means a sync cycle is currently running against the
	// repository, from the initial fetch through the final push.
	PhaseSyncing = "syncing"
	// PhaseConflict means the repository has conflict backup files that
	// need a human's attention (see internal/daemon/conflict.go), or a
	// rebase/merge that predates gitloop's own cycle is in progress.
	PhaseConflict = "conflict"
)

// RepoStatus is one repository's last-known sync state, as reported by
// `gitloop status`.
type RepoStatus struct {
	Path string `json:"path"`
	// Phase is one of PhaseIdle, PhaseSyncing, or PhaseConflict.
	Phase      string `json:"phase"`
	LastCommit string `json:"last_commit,omitempty"`
	LastPush   string `json:"last_push,omitempty"`
	// LastSuccessfulSyncAt is stamped only when a full cycle (fetch through
	// push) completes without error, so a long-silent value here — as
	// opposed to LastError, which only reflects the most recent cycle — is
	// a sign that syncing has been silently broken (e.g. expired push
	// credentials) for a while.
	LastSuccessfulSyncAt time.Time `json:"last_successful_sync_at,omitempty"`
	LastError            string    `json:"last_error,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// StatusFile is the on-disk shape gitloop writes so `gitloop status` (a
// separate process) can read it without talking to the running daemon.
type StatusFile struct {
	// Pid is the daemon process's own process ID, written once at startup.
	Pid int `json:"pid"`
	// LastHeartbeatAt is stamped on a fixed interval by a goroutine
	// independent of any repository's sync loop (see Daemon.runHeartbeat),
	// so a consumer of this file can tell the daemon process itself is
	// still alive — as opposed to merely having synced recently — even
	// while every repository loop is idle, backed off, or blocked.
	LastHeartbeatAt time.Time             `json:"last_heartbeat_at"`
	Repos           map[string]RepoStatus `json:"repos"`
}

// DefaultStatusPath returns the default location gitloop records status to:
// ~/Library/Application Support/gitloop/status.json. Application Support is
// used rather than ~/Library/Caches because macOS may purge the latter at
// any time, and this file is meant to be durable enough for another process
// to rely on for crash/staleness detection.
func DefaultStatusPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "gitloop", "status.json"), nil
}

// LoadStatusFile reads the status file at path, returning an empty one if it
// doesn't exist yet.
func LoadStatusFile(path string) (*StatusFile, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &StatusFile{Repos: map[string]RepoStatus{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var sf StatusFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, err
	}
	if sf.Repos == nil {
		sf.Repos = map[string]RepoStatus{}
	}
	return &sf, nil
}

// Save writes the status file atomically (write to a temp file, then
// rename) so `gitloop status` never observes a half-written file.
func (sf *StatusFile) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// statusRecorder serializes updates to a shared StatusFile from the
// multiple per-repository goroutines and persists it to disk after each
// change.
type statusRecorder struct {
	mu   sync.Mutex
	path string
	file *StatusFile
}

func newStatusRecorder(path string) (*statusRecorder, error) {
	sf, err := LoadStatusFile(path)
	if err != nil {
		return nil, err
	}
	return &statusRecorder{path: path, file: sf}, nil
}

// update applies mutate to repoPath's status entry, stamps it with the
// current time, and persists the file. Save errors are swallowed (status
// reporting is best-effort and must never take down a sync loop) but
// returned to the caller so it can log if it wants to.
func (r *statusRecorder) update(repoPath string, mutate func(*RepoStatus)) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	st, seen := r.file.Repos[repoPath]
	if !seen {
		st.Phase = PhaseIdle
	}
	st.Path = repoPath
	mutate(&st)
	st.UpdatedAt = time.Now()
	r.file.Repos[repoPath] = st

	return r.file.Save(r.path)
}

// setPid stamps the status file with the daemon's own process ID. It is
// written once at startup so a consumer can compare it against the live
// process table to detect a crashed daemon whose last status update is
// stale.
func (r *statusRecorder) setPid(pid int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.file.Pid = pid
	return r.file.Save(r.path)
}

// heartbeat stamps the current time as the daemon's last-known-alive
// timestamp.
func (r *statusRecorder) heartbeat() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.file.LastHeartbeatAt = time.Now()
	return r.file.Save(r.path)
}
