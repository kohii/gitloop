package gitcmd

// Runner will execute git subcommands against a repository directory.
//
// TODO: implement (add, commit, fetch, rev-list, rebase, push, and the
// in-progress rebase/merge guard checks).
type Runner struct {
	// Dir is the repository's working directory.
	Dir string
}
