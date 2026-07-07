package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// OnConflict selects how gitloop resolves a real (non-fast-forwardable)
// rebase conflict.
type OnConflict string

const (
	// OnConflictClaude resolves conflicts with `claude -p`, falling back to
	// OnConflictBackup if the claude CLI or an API key isn't available, or
	// if it fails to produce a marker-free file.
	OnConflictClaude OnConflict = "claude"
	// OnConflictBackup preserves both sides of a conflict as separate files
	// and aborts the rebase, leaving resolution to the user.
	OnConflictBackup OnConflict = "backup"
)

// Defaults are applied to any Repository field left unset in the config
// file.
type Defaults struct {
	Settle        time.Duration
	MaxWait       time.Duration
	FetchInterval time.Duration
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
	Remote:        "origin",
	Branch:        "",
	OnConflict:    OnConflictClaude,
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
	// FetchInterval is how often gitloop fetches the remote even without
	// local file activity, to pick up changes made elsewhere.
	FetchInterval time.Duration
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
	// the same working tree. It defaults to
	// "<repo path>/.notesapp/state/save.lock"; an empty string disables the
	// coordination entirely, for repositories with no such external writer.
	SaveLockPath string
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
	Remote        string  `yaml:"remote"`
	Branch        string  `yaml:"branch"`
	OnConflict    string  `yaml:"on_conflict"`
	SaveLockPath  *string `yaml:"save_lock_path"`
}

type rawDefaults struct {
	Settle        string  `yaml:"settle"`
	MaxWait       string  `yaml:"max_wait"`
	FetchInterval string  `yaml:"fetch_interval"`
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
	repo.SaveLockPath = resolveSaveLockPath(raw.SaveLockPath, defaults.SaveLockPath, path)

	return repo, nil
}

// resolveSaveLockPath applies save_lock_path's three-level precedence: an
// explicit per-repository value (raw, including an explicit "" to disable)
// wins; failing that, an explicit defaults-block value (fallback) wins;
// failing that, the dynamic per-repository default is used. A plain string
// default can't represent "unset" here because "" is itself a meaningful,
// explicit value (disable locking), hence the pointer inputs.
func resolveSaveLockPath(raw, fallback *string, repoPath string) string {
	if raw != nil {
		return *raw
	}
	if fallback != nil {
		return *fallback
	}
	return filepath.Join(repoPath, ".notesapp", "state", "save.lock")
}

func parseDurationOr(raw string, fallback time.Duration, field string) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", field, raw, err)
	}
	return d, nil
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
