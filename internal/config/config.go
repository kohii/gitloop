package config

import (
	"bytes"
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
	// ModeSync auto-commits, then fetches, integrates, and pushes against the
	// configured remote. It is also the parsed value for an omitted mode until
	// the daemon checks whether the repository has any remotes.
	ModeSync Mode = "sync"
	// ModeCommitOnly stops the cycle after the auto-commit phase — no fetch,
	// no merge, no push. It is used for repositories with no remote when mode
	// was omitted, as well as for repositories where users select it explicitly.
	ModeCommitOnly Mode = "commit-only"
	// ModeCommittedSync only transports commits that already exist in the
	// repository. It never stages or creates a commit, fast-forwards only when
	// the working tree is clean, and refuses to merge divergent histories.
	ModeCommittedSync Mode = "committed-sync"
)

// WorkflowType is the explicit workflow contract for a repository. The
// legacy mode field remains supported, but new configurations can use a
// tagged workflow so that invalid combinations are rejected at parse time.
type WorkflowType string

const (
	WorkflowAutoCommitSync WorkflowType = "auto-commit-sync"
	WorkflowAutoCommitOnly WorkflowType = "auto-commit-only"
	WorkflowCommittedSync  WorkflowType = "committed-sync"
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
	RemoteTimeout time.Duration
	Mode          Mode
	Remote        string
	Branch        string
	OnConflict    OnConflict
	// SaveLockPath is nil unless the config file's top-level "defaults"
	// block sets save_lock_path explicitly (including to ""), in which case
	// it overrides each repository's dynamic per-path default. See
	// Repository.SaveLockPath.
	SaveLockPath *string
	// modeExplicit records whether defaults.mode was present in the config.
	// It is copied into each repository so the daemon can distinguish an
	// omitted mode from an explicit mode.
	modeExplicit bool
}

// builtinDefaults are used for anything the config file's own top-level
// "defaults" section leaves unset. They match the values documented in
// README.md; keep the two in sync.
var builtinDefaults = Defaults{
	Settle:        3 * time.Second,
	MaxWait:       60 * time.Second,
	FetchInterval: 30 * time.Second,
	RemoteTimeout: 10 * time.Minute,
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
	// file activity: under a remote workflow it fetches changes made elsewhere,
	// and under an auto-commit-only workflow it catches working-tree changes the
	// file watcher missed.
	FetchInterval time.Duration
	// RemoteTimeout is the hard deadline for each fetch or push. It does not
	// apply to local Git operations that could leave the working tree or index
	// half-mutated if interrupted.
	RemoteTimeout time.Duration
	// Mode is the legacy workflow selector. Under ModeCommitOnly, Remote and
	// Branch are unused; ModeCommittedSync uses them without auto-committing.
	Mode Mode
	// Workflow is the explicit workflow contract. It is populated for parsed
	// configurations and may be left empty on hand-built repositories, in
	// which case Mode determines the behavior for backwards compatibility.
	Workflow WorkflowType
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
	// modeExplicit is true when mode was set on this repository or in the
	// top-level defaults block. It is parser provenance used by the daemon,
	// not a runtime configuration knob.
	modeExplicit bool
	// workflowExplicit is true when a nested workflow was present in the
	// config. An explicitly selected workflow must not be auto-downgraded.
	workflowExplicit bool
}

// ModeWasExplicitlySet reports whether this repository's effective mode came
// from an explicit mode setting in the config file rather than the built-in
// default.
func (r Repository) ModeWasExplicitlySet() bool {
	return r.modeExplicit
}

// WorkflowWasExplicitlySet reports whether this repository's workflow came
// from a nested workflow setting in the config file.
func (r Repository) WorkflowWasExplicitlySet() bool {
	return r.workflowExplicit
}

// SyncsRemote reports whether this repository fetches, integrates, and
// pushes after its local phase. Every mode but ModeCommitOnly does,
// including the zero value — Parse always fills Mode in, so an empty Mode
// means a hand-built Repository, and a forgotten field shouldn't silently
// disable someone's sync.
func (r Repository) SyncsRemote() bool {
	return r.effectiveWorkflow() != WorkflowAutoCommitOnly
}

// AutoCommits reports whether a cycle may stage and create a commit from the
// working tree. Committed-sync repositories leave all working-tree and index
// changes under the user's control.
func (r Repository) AutoCommits() bool {
	return r.effectiveWorkflow() != WorkflowCommittedSync
}

// IsCommittedSync reports whether this repository transports only commits
// that were created by a human or another external process.
func (r Repository) IsCommittedSync() bool {
	return r.effectiveWorkflow() == WorkflowCommittedSync
}

func (r Repository) effectiveWorkflow() WorkflowType {
	if r.Workflow != "" {
		return r.Workflow
	}
	switch r.Mode {
	case ModeCommitOnly:
		return WorkflowAutoCommitOnly
	case ModeCommittedSync:
		return WorkflowCommittedSync
	default:
		return WorkflowAutoCommitSync
	}
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
	Path          string       `yaml:"path"`
	Settle        string       `yaml:"settle"`
	MaxWait       string       `yaml:"max_wait"`
	FetchInterval string       `yaml:"fetch_interval"`
	RemoteTimeout string       `yaml:"remote_timeout"`
	Mode          string       `yaml:"mode"`
	Remote        string       `yaml:"remote"`
	Branch        string       `yaml:"branch"`
	OnConflict    string       `yaml:"on_conflict"`
	SaveLockPath  *string      `yaml:"save_lock_path"`
	Workflow      *rawWorkflow `yaml:"workflow"`
}

type rawWorkflow struct {
	Type          string  `yaml:"type"`
	Remote        string  `yaml:"remote"`
	Branch        string  `yaml:"branch"`
	Interval      string  `yaml:"interval"`
	Settle        string  `yaml:"settle"`
	MaxWait       string  `yaml:"max_wait"`
	OnConflict    string  `yaml:"on_conflict"`
	SaveLockPath  *string `yaml:"save_lock_path"`
	RemoteTimeout string  `yaml:"remote_timeout"`
}

type rawDefaults struct {
	Settle        string  `yaml:"settle"`
	MaxWait       string  `yaml:"max_wait"`
	FetchInterval string  `yaml:"fetch_interval"`
	RemoteTimeout string  `yaml:"remote_timeout"`
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
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
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
	if d.RemoteTimeout, err = parseDurationOr(raw.RemoteTimeout, d.RemoteTimeout, "remote_timeout"); err != nil {
		return Defaults{}, err
	}
	if raw.Mode != "" {
		m, err := parseMode(raw.Mode)
		if err != nil {
			return Defaults{}, err
		}
		d.Mode = m
		d.modeExplicit = true
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
		Path:             path,
		Settle:           defaults.Settle,
		MaxWait:          defaults.MaxWait,
		FetchInterval:    defaults.FetchInterval,
		RemoteTimeout:    defaults.RemoteTimeout,
		Mode:             defaults.Mode,
		Workflow:         workflowForMode(defaults.Mode),
		Remote:           defaults.Remote,
		Branch:           defaults.Branch,
		OnConflict:       defaults.OnConflict,
		modeExplicit:     defaults.modeExplicit || raw.Mode != "",
		workflowExplicit: raw.Workflow != nil,
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
	if repo.RemoteTimeout, err = parseDurationOr(raw.RemoteTimeout, repo.RemoteTimeout, "remote_timeout"); err != nil {
		return Repository{}, err
	}
	if raw.Mode != "" {
		m, err := parseMode(raw.Mode)
		if err != nil {
			return Repository{}, err
		}
		repo.Mode = m
		repo.Workflow = workflowForMode(m)
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
	repo.SaveLockPath, err = resolveSaveLockPath(raw.SaveLockPath, defaults.SaveLockPath)
	if err != nil {
		return Repository{}, err
	}
	if repo.IsCommittedSync() && raw.Workflow == nil {
		if err := validateCommittedSyncLegacyFields(raw); err != nil {
			return Repository{}, err
		}
	}
	if repo.Mode == ModeCommitOnly && raw.Workflow == nil && raw.RemoteTimeout != "" {
		return Repository{}, fmt.Errorf("mode %q does not accept remote_timeout", ModeCommitOnly)
	}

	if raw.Workflow != nil {
		if raw.Mode != "" {
			return Repository{}, fmt.Errorf("workflow cannot be combined with mode")
		}
		workflow, err := parseWorkflow(raw.Workflow.Type)
		if err != nil {
			return Repository{}, err
		}
		var workflowErr error
		repo, workflowErr = applyWorkflow(repo, raw, raw.Workflow, workflow)
		if workflowErr != nil {
			return Repository{}, workflowErr
		}
		repo.Workflow = workflow
		switch workflow {
		case WorkflowAutoCommitSync:
			repo.Mode = ModeSync
		case WorkflowAutoCommitOnly:
			repo.Mode = ModeCommitOnly
		case WorkflowCommittedSync:
			repo.Mode = ModeCommittedSync
		}
	}

	return repo, nil
}

func workflowForMode(mode Mode) WorkflowType {
	switch mode {
	case ModeCommitOnly:
		return WorkflowAutoCommitOnly
	case ModeCommittedSync:
		return WorkflowCommittedSync
	default:
		return WorkflowAutoCommitSync
	}
}

func parseWorkflow(raw string) (WorkflowType, error) {
	w := WorkflowType(raw)
	switch w {
	case WorkflowAutoCommitSync, WorkflowAutoCommitOnly, WorkflowCommittedSync:
		return w, nil
	default:
		return "", fmt.Errorf("workflow.type: unknown value %q (want %q, %q, or %q)", raw, WorkflowAutoCommitSync, WorkflowAutoCommitOnly, WorkflowCommittedSync)
	}
}

func validateCommittedSyncLegacyFields(raw rawRepository) error {
	if raw.Settle != "" || raw.MaxWait != "" || raw.OnConflict != "" {
		return fmt.Errorf("mode %q does not accept settle, max_wait, or on_conflict", ModeCommittedSync)
	}
	return nil
}

// applyWorkflow applies the nested workflow overrides and rejects fields that
// would be meaningless for the selected workflow. The flat legacy fields are
// still accepted for shared defaults, but a repository-level duplicate is
// rejected when the nested form also specifies the same value.
func applyWorkflow(repo Repository, raw rawRepository, wf *rawWorkflow, workflow WorkflowType) (Repository, error) {
	if workflow == WorkflowAutoCommitOnly {
		if wf.Remote != "" || wf.Branch != "" || wf.OnConflict != "" || wf.RemoteTimeout != "" || raw.Remote != "" || raw.Branch != "" || raw.OnConflict != "" || raw.RemoteTimeout != "" {
			return Repository{}, fmt.Errorf("workflow.type %q does not accept remote, branch, remote_timeout, or on_conflict", workflow)
		}
	}
	if workflow == WorkflowCommittedSync && (raw.Settle != "" || raw.MaxWait != "" || raw.OnConflict != "" || wf.Settle != "" || wf.MaxWait != "" || wf.OnConflict != "") {
		return Repository{}, fmt.Errorf("workflow.type %q does not accept settle, max_wait, or on_conflict", workflow)
	}

	if wf.Remote != "" {
		if raw.Remote != "" {
			return Repository{}, fmt.Errorf("workflow.remote cannot be combined with remote")
		}
		repo.Remote = wf.Remote
	}
	if wf.Branch != "" {
		if raw.Branch != "" {
			return Repository{}, fmt.Errorf("workflow.branch cannot be combined with branch")
		}
		repo.Branch = wf.Branch
	}
	if wf.Interval != "" {
		if raw.FetchInterval != "" {
			return Repository{}, fmt.Errorf("workflow.interval cannot be combined with fetch_interval")
		}
		d, err := parseDurationOr(wf.Interval, repo.FetchInterval, "workflow.interval")
		if err != nil {
			return Repository{}, err
		}
		repo.FetchInterval = d
	}
	if wf.RemoteTimeout != "" {
		if raw.RemoteTimeout != "" {
			return Repository{}, fmt.Errorf("workflow.remote_timeout cannot be combined with remote_timeout")
		}
		d, err := parseDurationOr(wf.RemoteTimeout, repo.RemoteTimeout, "workflow.remote_timeout")
		if err != nil {
			return Repository{}, err
		}
		repo.RemoteTimeout = d
	}
	if wf.Settle != "" {
		if raw.Settle != "" {
			return Repository{}, fmt.Errorf("workflow.settle cannot be combined with settle")
		}
		d, err := parseDurationOr(wf.Settle, repo.Settle, "workflow.settle")
		if err != nil {
			return Repository{}, err
		}
		repo.Settle = d
	}
	if wf.MaxWait != "" {
		if raw.MaxWait != "" {
			return Repository{}, fmt.Errorf("workflow.max_wait cannot be combined with max_wait")
		}
		d, err := parseDurationOr(wf.MaxWait, repo.MaxWait, "workflow.max_wait")
		if err != nil {
			return Repository{}, err
		}
		repo.MaxWait = d
	}
	if wf.OnConflict != "" {
		if raw.OnConflict != "" {
			return Repository{}, fmt.Errorf("workflow.on_conflict cannot be combined with on_conflict")
		}
		oc, err := parseOnConflict(wf.OnConflict)
		if err != nil {
			return Repository{}, err
		}
		repo.OnConflict = oc
	}
	if wf.SaveLockPath != nil {
		if raw.SaveLockPath != nil {
			return Repository{}, fmt.Errorf("workflow.save_lock_path cannot be combined with save_lock_path")
		}
		expanded, err := resolveSaveLockPath(wf.SaveLockPath, nil)
		if err != nil {
			return Repository{}, fmt.Errorf("workflow.save_lock_path: %w", err)
		}
		repo.SaveLockPath = expanded
	}
	return repo, nil
}

// resolveSaveLockPath applies save_lock_path's precedence: an explicit
// per-repository value (raw, including an explicit "" to disable) wins;
// failing that, an explicit defaults-block value (fallback) wins; failing
// that, save-lock coordination is off ("" = disabled). Pointer inputs are
// needed so callers can distinguish "unset" from an explicit empty string.
func resolveSaveLockPath(raw, fallback *string) (string, error) {
	var path string
	if raw != nil {
		path = *raw
	} else if fallback != nil {
		path = *fallback
	}
	if path == "" {
		return "", nil
	}
	return expandHome(path)
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
	case ModeSync, ModeCommitOnly, ModeCommittedSync:
		return Mode(raw), nil
	default:
		return "", fmt.Errorf("mode: unknown value %q (want %q, %q, or %q)", raw, ModeSync, ModeCommitOnly, ModeCommittedSync)
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
