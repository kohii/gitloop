// Package statemachine classifies the relationship between a local branch and
// its remote counterpart and decides the sync action to take (no-op, push,
// fast-forward merge, or rebase-then-push), independent of any execution
// environment.
package statemachine
