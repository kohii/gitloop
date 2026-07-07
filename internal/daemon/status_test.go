package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultStatusPathUsesApplicationSupport(t *testing.T) {
	path, err := DefaultStatusPath()
	if err != nil {
		t.Fatalf("DefaultStatusPath: %v", err)
	}
	if !strings.Contains(path, filepath.Join("Library", "Application Support", "gitloop")) {
		t.Errorf("DefaultStatusPath() = %q, want it under Library/Application Support/gitloop", path)
	}
	if strings.Contains(path, "Caches") {
		t.Errorf("DefaultStatusPath() = %q, want it to avoid Library/Caches (macOS may purge it)", path)
	}
}

func TestStatusRecorderSetPidAndHeartbeatPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	r, err := newStatusRecorder(path)
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	if err := r.setPid(4242); err != nil {
		t.Fatalf("setPid: %v", err)
	}
	if err := r.heartbeat(); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	sf, err := LoadStatusFile(path)
	if err != nil {
		t.Fatalf("LoadStatusFile: %v", err)
	}
	if sf.Pid != 4242 {
		t.Errorf("Pid = %d, want 4242", sf.Pid)
	}
	if sf.LastHeartbeatAt.IsZero() {
		t.Error("LastHeartbeatAt is zero, want it stamped")
	}
}

func TestStatusRecorderUpdateDefaultsNewEntryToIdle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	r, err := newStatusRecorder(path)
	if err != nil {
		t.Fatalf("newStatusRecorder: %v", err)
	}

	repoPath := "/repo"
	if err := r.update(repoPath, func(s *RepoStatus) { s.LastCommit = "now" }); err != nil {
		t.Fatalf("update: %v", err)
	}

	sf, err := LoadStatusFile(path)
	if err != nil {
		t.Fatalf("LoadStatusFile: %v", err)
	}
	if got := sf.Repos[repoPath].Phase; got != PhaseIdle {
		t.Errorf("Phase = %q, want %q for a repo's first status update", got, PhaseIdle)
	}
}

func TestStatusFileRoundTripsNewFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	sf := &StatusFile{
		Pid: 99,
		Repos: map[string]RepoStatus{
			"/repo": {Path: "/repo", Phase: PhaseConflict},
		},
	}
	if err := sf.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"pid"`, `"last_heartbeat_at"`, `"phase": "conflict"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("status file JSON missing %q:\n%s", want, data)
		}
	}

	got, err := LoadStatusFile(path)
	if err != nil {
		t.Fatalf("LoadStatusFile: %v", err)
	}
	if got.Pid != 99 || got.Repos["/repo"].Phase != PhaseConflict {
		t.Errorf("round-tripped StatusFile = %+v, want Pid=99 and phase=conflict", got)
	}
}
