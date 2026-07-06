// Package commitmsg builds the auto-commit messages gitloop writes when it
// commits working-tree changes on a user's behalf. Messages are built as a
// pure function of the change set so they are easy to test and so the same
// format is used regardless of which repository or trigger produced the
// commit.
package commitmsg
