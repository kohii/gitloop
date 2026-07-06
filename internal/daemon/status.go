package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RepoStatus is one repository's last-known sync state, as reported by
// `gitloop status`.
type RepoStatus struct {
	Path       string    `json:"path"`
	LastCommit string    `json:"last_commit,omitempty"`
	LastPush   string    `json:"last_push,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// StatusFile is the on-disk shape gitloop writes so `gitloop status` (a
// separate process) can read it without talking to the running daemon.
type StatusFile struct {
	Repos map[string]RepoStatus `json:"repos"`
}

// DefaultStatusPath returns the default location gitloop records status to:
// ~/Library/Caches/gitloop/status.json.
func DefaultStatusPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Caches", "gitloop", "status.json"), nil
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

	st := r.file.Repos[repoPath]
	st.Path = repoPath
	mutate(&st)
	st.UpdatedAt = time.Now()
	r.file.Repos[repoPath] = st

	return r.file.Save(r.path)
}
