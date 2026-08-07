package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode selects how much of the sync cycle a repository runs.
type Mode string

const (
	// ModeSync is the default: auto-commit, then fetch, integrate, and push
	// against the configured remote.
	ModeSync Mode = "sync"
	// ModeCommitOnly stops the cycle after the auto-commit phase — no fetch,
	// no merge, no push. It's for repositories with no remote at all, which
	// under ModeSync would fail `git fetch` every cycle and so never report
	// a healthy status. See docs/design.md for why it's an explicit opt-in
	// rather than inferred from an unresolvable remote.
	ModeCommitOnly Mode = "commit-only"
)

// OnConflict selects how gitloop resolves a real (non-fast-forwardable)
// merge conflict.
type OnConflict string

const (
	// OnConflictBackup is the default: preserve both sides of a conflict as
	// separate files next to the original, accept the upstream (`theirs`)
	// side, and complete the merge commit. Nothing is lost — the discarded
	// local content lives on in the .ours.* backup file. Chosen as default
	// because it never fails silently when an external dependency (the
	// claude CLI, ANTHROPIC_API_KEY) is missing or expired.
	OnConflictBackup OnConflict = "backup"
	// OnConflictClaude is opt-in: try `claude -p` to resolve the markers,
	// falling back to OnConflictBackup if the CLI or API key isn't
	// available, or if the model leaves markers in the file. Requires
	// explicit configuration since silent AI failure is otherwise hard to
	// notice.
	OnConflictClaude OnConflict = "claude"
)

// Defaults are applied to any Repository field left unset in the config
// file.
type Defaults struct {
	Settle        time.Duration
	MaxWait       time.Duration
	FetchInterval time.Duration
	Mode          Mode
	Remote        string
	Branch        string
	OnConflict    OnConflict
	// SaveLockPath is nil unless the config file's top-level "defaults"
	// block sets save_lock_path explicitly (including to ""), in which case
	// it overrides each repository's dynamic per-path default. See
	// Repository.SaveLockPath.
	SaveLockPath *string
}

// builtinDefaults are used for anything the config file's own top-level
// "defaults" section leaves unset. They match the values documented in
// README.md; keep the two in sync.
var builtinDefaults = Defaults{
	Settle:        3 * time.Second,
	MaxWait:       60 * time.Second,
	FetchInterval: 5 * time.Minute,
	Mode:          ModeSync,
	Remote:        "origin",
	Branch:        "",
	OnConflict:    OnConflictBackup,
}

// Repository is one repository gitloop watches, with all defaults resolved.
type Repository struct {
	// Path is the repository's working directory, with "~" expanded.
	Path string
	// Settle is how long the file watcher waits for changes to stop before
	// auto-committing.
	Settle time.Duration
	// MaxWait is the longest gitloop will delay a commit while changes keep
	// arriving before settle is reached.
	MaxWait time.Duration
	// FetchInterval is how often gitloop runs a cycle even without local
	// file activity: under ModeSync that fetch is what picks up changes made
	// elsewhere, and under ModeCommitOnly it is the safety net that catches
	// working-tree changes the file watcher missed.
	FetchInterval time.Duration
	// Mode selects whether the cycle talks to a remote at all. Under
	// ModeCommitOnly, Remote and Branch are unused.
	Mode Mode
	// Remote is the git remote name to fetch from and push to.
	Remote string
	// Branch is the branch to sync. Empty means "whatever is currently
	// checked out", resolved at runtime.
	Branch string
	// OnConflict selects the conflict-resolution policy.
	OnConflict OnConflict
	// SaveLockPath is the advisory lock file gitloop tries to hold (via
	// flock) before starting each sync cycle, to coordinate with an
	// external process (e.g. a notes-app server) that may be mid-save on
	// the same working tree. Empty (the default) disables the coordination
	// entirely; the external writer's config is responsible for pointing
	// gitloop at whatever lock file they both agree to hold.
	SaveLockPath string
}

// SyncsRemote reports whether this repository fetches, integrates, and
// pushes after the auto-commit phase. Every mode but ModeCommitOnly does,
// including the zero value — Parse always fills Mode in, so an empty Mode
// means a hand-built Repository, and a forgotten field shouldn't silently
// disable someone's sync.
func (r Repository) SyncsRemote() bool {
	return r.Mode != ModeCommitOnly
}

// Config is gitloop's fully resolved configuration: every repository has all
// defaults applied and paths expanded.
type Config struct {
	Repositories []Repository
	Defaults     Defaults
}

// rawConfig mirrors the YAML file shape before defaults are applied. All
// duration-like fields are read as strings so they can be parsed with
// time.ParseDuration.
type rawConfig struct {
	Repositories []rawRepository `yaml:"repositories"`
	Defaults     rawDefaults     `yaml:"defaults"`
}

type rawRepository struct {
	Path          string  `yaml:"path"`
	Settle        string  `yaml:"settle"`
	MaxWait       string  `yaml:"max_wait"`
	FetchInterval string  `yaml:"fetch_interval"`
	Mode          string  `yaml:"mode"`
	Remote        string  `yaml:"remote"`
	Branch        string  `yaml:"branch"`
	OnConflict    string  `yaml:"on_conflict"`
	SaveLockPath  *string `yaml:"save_lock_path"`
}

type rawDefaults struct {
	Settle        string  `yaml:"settle"`
	MaxWait       string  `yaml:"max_wait"`
	FetchInterval string  `yaml:"fetch_interval"`
	Mode          string  `yaml:"mode"`
	Remote        string  `yaml:"remote"`
	Branch        string  `yaml:"branch"`
	OnConflict    string  `yaml:"on_conflict"`
	SaveLockPath  *string `yaml:"save_lock_path"`
}

// Load reads, parses, and validates the gitloop config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	return Parse(data)
}

// Parse parses config file contents already read into memory. It is
// exposed separately from Load so callers (and tests) don't need a file on
// disk.
func Parse(data []byte) (*Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config: parsing YAML: %w", err)
	}
	if len(raw.Repositories) == 0 {
		return nil, fmt.Errorf("config: at least one repository is required")
	}

	defaults, err := resolveDefaults(raw.Defaults)
	if err != nil {
		return nil, fmt.Errorf("config: defaults: %w", err)
	}

	repos := make([]Repository, 0, len(raw.Repositories))
	for i, rr := range raw.Repositories {
		repo, err := resolveRepository(rr, defaults)
		if err != nil {
			return nil, fmt.Errorf("config: repositories[%d]: %w", i, err)
		}
		repos = append(repos, repo)
	}

	return &Config{Repositories: repos, Defaults: defaults}, nil
}

func resolveDefaults(raw rawDefaults) (Defaults, error) {
	d := builtinDefaults

	var err error
	if d.Settle, err = parseDurationOr(raw.Settle, d.Settle, "settle"); err != nil {
		return Defaults{}, err
	}
	if d.MaxWait, err = parseDurationOr(raw.MaxWait, d.MaxWait, "max_wait"); err != nil {
		return Defaults{}, err
	}
	if d.FetchInterval, err = parseDurationOr(raw.FetchInterval, d.FetchInterval, "fetch_interval"); err != nil {
		return Defaults{}, err
	}
	if raw.Mode != "" {
		m, err := parseMode(raw.Mode)
		if err != nil {
			return Defaults{}, err
		}
		d.Mode = m
	}
	if raw.Remote != "" {
		d.Remote = raw.Remote
	}
	if raw.Branch != "" {
		d.Branch = raw.Branch
	}
	if raw.OnConflict != "" {
		oc, err := parseOnConflict(raw.OnConflict)
		if err != nil {
			return Defaults{}, err
		}
		d.OnConflict = oc
	}
	d.SaveLockPath = raw.SaveLockPath
	return d, nil
}

func resolveRepository(raw rawRepository, defaults Defaults) (Repository, error) {
	if raw.Path == "" {
		return Repository{}, fmt.Errorf("path is required")
	}
	path, err := expandHome(raw.Path)
	if err != nil {
		return Repository{}, fmt.Errorf("path %q: %w", raw.Path, err)
	}

	repo := Repository{
		Path:          path,
		Settle:        defaults.Settle,
		MaxWait:       defaults.MaxWait,
		FetchInterval: defaults.FetchInterval,
		Mode:          defaults.Mode,
		Remote:        defaults.Remote,
		Branch:        defaults.Branch,
		OnConflict:    defaults.OnConflict,
	}

	if repo.Settle, err = parseDurationOr(raw.Settle, repo.Settle, "settle"); err != nil {
		return Repository{}, err
	}
	if repo.MaxWait, err = parseDurationOr(raw.MaxWait, repo.MaxWait, "max_wait"); err != nil {
		return Repository{}, err
	}
	if repo.FetchInterval, err = parseDurationOr(raw.FetchInterval, repo.FetchInterval, "fetch_interval"); err != nil {
		return Repository{}, err
	}
	if raw.Mode != "" {
		m, err := parseMode(raw.Mode)
		if err != nil {
			return Repository{}, err
		}
		repo.Mode = m
	}
	if raw.Remote != "" {
		repo.Remote = raw.Remote
	}
	if raw.Branch != "" {
		repo.Branch = raw.Branch
	}
	if raw.OnConflict != "" {
		oc, err := parseOnConflict(raw.OnConflict)
		if err != nil {
			return Repository{}, err
		}
		repo.OnConflict = oc
	}
	repo.SaveLockPath = resolveSaveLockPath(raw.SaveLockPath, defaults.SaveLockPath)

	return repo, nil
}

// resolveSaveLockPath applies save_lock_path's precedence: an explicit
// per-repository value (raw, including an explicit "" to disable) wins;
// failing that, an explicit defaults-block value (fallback) wins; failing
// that, save-lock coordination is off ("" = disabled). Pointer inputs are
// needed so callers can distinguish "unset" from an explicit empty string.
func resolveSaveLockPath(raw, fallback *string) string {
	if raw != nil {
		return *raw
	}
	if fallback != nil {
		return *fallback
	}
	return ""
}

func parseDurationOr(raw string, fallback time.Duration, field string) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", field, raw, err)
	}
	// Every duration in this config drives a timer or ticker, and
	// time.NewTicker panics on a non-positive interval. Rejecting it here
	// makes it a startup error the user sees immediately, instead of a
	// recovered panic that leaves one repository permanently unwatched.
	if d <= 0 {
		return 0, fmt.Errorf("%s: must be positive, got %q", field, raw)
	}
	return d, nil
}

func parseMode(raw string) (Mode, error) {
	switch Mode(raw) {
	case ModeSync, ModeCommitOnly:
		return Mode(raw), nil
	default:
		return "", fmt.Errorf("mode: unknown value %q (want %q or %q)", raw, ModeSync, ModeCommitOnly)
	}
}

func parseOnConflict(raw string) (OnConflict, error) {
	switch OnConflict(raw) {
	case OnConflictClaude, OnConflictBackup:
		return OnConflict(raw), nil
	default:
		return "", fmt.Errorf("on_conflict: unknown value %q (want %q or %q)", raw, OnConflictClaude, OnConflictBackup)
	}
}

// expandHome expands a leading "~" or "~/..." to the current user's home
// directory. Paths that don't start with "~" are returned unchanged (after
// being cleaned).
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return filepath.Clean(path), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

// DefaultPath returns the default config file location,
// ~/.config/gitloop/config.yaml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gitloop", "config.yaml"), nil
}
