// Package daemon watches a repository's working tree for file changes and
// runs the sync loop (debounce, fetch, and applying statemachine decisions)
// as a long-running process.
package daemon
