package daemon

// Daemon will hold the file watcher, debounce timer, and sync loop state.
//
// TODO: implement file watching, settle-based debounce with a max-wait cap,
// and periodic fetch triggers as described in the design doc.
type Daemon struct{}
