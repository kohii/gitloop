package daemon

import "sync"

// triggerReason names what asked for a sync cycle out of band — that is,
// neither a file-watcher event nor the fetch interval. It is carried through
// to the cycle's log line so a cycle can be traced back to what caused it.
type triggerReason string

const (
	// TriggerManual is a person or a script asking for a cycle now, via
	// `gitloop sync`.
	TriggerManual triggerReason = "manual"
	// TriggerWake is the machine having come back from sleep.
	TriggerWake triggerReason = "wake"
	// TriggerNetwork is this machine's network attachment having changed.
	TriggerNetwork triggerReason = "network"
)

// isEnvironmental reports whether a reason came from the machine's
// surroundings rather than from someone asking. The difference matters when
// the file watcher is mid-debounce: a person asking for a sync has decided
// the working tree is worth committing as it stands, and the environment has
// no opinion about that.
func (r triggerReason) isEnvironmental() bool {
	return r == TriggerWake || r == TriggerNetwork
}

// repoTrigger is one repository's mailbox for out-of-band cycle requests.
//
// It holds at most one request, because what matters is that a cycle runs
// after the request, not that every request gets one of its own: ten arriving
// while a cycle is in flight collapse into a single follow-up.
//
// It is owned by the Daemon rather than by the watch loop, so that it
// outlives a loop that failed and is waiting out its retry backoff. A request
// that arrives during that window is still there when the loop comes back,
// instead of having been sent to a channel nobody reads any more.
type repoTrigger struct {
	mu      sync.Mutex
	pending triggerReason
	// signal only says "there is something in pending". Keeping the reason
	// beside it rather than in it is what lets fire decide what a second
	// request does to a first.
	signal chan struct{}
}

func newRepoTrigger() *repoTrigger {
	return &repoTrigger{signal: make(chan struct{}, 1)}
}

// fire asks for a cycle. It never blocks, so a caller holding a connection
// open cannot stall on a repository whose loop is busy.
//
// A request already waiting normally stands, since one cycle answers both.
// The exception is a person's request arriving behind an environment event:
// the loop treats the two differently — an environment event defers to a
// debounce in progress, a person's request does not — so letting the
// environment event stand would have `gitloop sync` report a cycle that then
// waits out the settle window it was meant to pre-empt.
func (t *repoTrigger) fire(reason triggerReason) {
	t.mu.Lock()
	if t.pending == "" || (t.pending.isEnvironmental() && !reason.isEnvironmental()) {
		t.pending = reason
	}
	t.mu.Unlock()

	select {
	case t.signal <- struct{}{}:
	default:
	}
}

// take removes and returns the waiting request, or "" if there is none.
func (t *repoTrigger) take() triggerReason {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	reason := t.pending
	t.pending = ""
	return reason
}

// signals is the channel a watch loop selects on. A nil trigger yields a nil
// channel, which in a select is simply never ready — that is how a loop with
// no out-of-band source runs.
func (t *repoTrigger) signals() <-chan struct{} {
	if t == nil {
		return nil
	}
	return t.signal
}
