package daemon

import "github.com/kohii/gitloop/internal/gitcmd"

// GitClient is the subset of gitcmd.Runner's behavior a repository's sync
// loop needs. It exists so tests can drive the loop against a fake instead
// of a real git checkout.
type GitClient interface {
	StatusPorcelain() ([]gitcmd.StatusEntry, error)
	AddAll() error
	AddPath(path string) error
	Commit(message string) error
	RemoteNames() ([]string, error)
	Fetch(remote string) error
	CurrentBranch() (string, error)
	RevListLeftRightCount(local, upstream string) (ahead, behind int, err error)
	MergeFF(upstream string) error
	Merge(upstream string) (conflict bool, err error)
	MergeAbort() error
	CheckoutTheirs(path string) error
	Push(remote, branch string) error
	ConflictedFiles() ([]string, error)
	ShowStage(stage int, path string) (content string, ok bool, err error)
}

// gitFactory builds a GitClient for a repository directory. Production code
// uses gitcmd.New; tests substitute a fake.
type gitFactory func(dir string) GitClient

func defaultGitFactory(dir string) GitClient {
	return gitcmd.New(dir)
}

var _ GitClient = (*gitcmd.Runner)(nil)
