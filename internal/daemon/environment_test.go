package daemon

import (
	"context"
	"testing"
	"time"
)

// startBroadcaster runs broadcastEnvironmentEvents with timings short enough
// for a test, and returns the channel its sources would write to.
func startBroadcaster(t *testing.T, triggers map[string]*repoTrigger, settleFor, cooldown time.Duration) chan triggerReason {
	t.Helper()
	events := make(chan triggerReason, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		broadcastEnvironmentEvents(ctx, events, triggers, settleFor, cooldown, discardLogger())
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("broadcastEnvironmentEvents did not shut down within 2s of context cancellation")
		}
	})
	return events
}

// TestBroadcastEnvironmentEventsTellsEveryRepository pins the fan-out:
// neither waking up nor reconnecting is about one repository, so all of them
// hear about it.
func TestBroadcastEnvironmentEventsTellsEveryRepository(t *testing.T) {
	triggers := testTriggers("/notes", "/journal")
	events := startBroadcaster(t, triggers, 10*time.Millisecond, time.Hour)

	events <- TriggerWake

	waitFor(t, 2*time.Second, func() bool { return pending(triggers) == 2 })
	for path, reason := range drain(triggers) {
		if reason != TriggerWake {
			t.Errorf("%s reason = %q, want %q", path, reason, TriggerWake)
		}
	}
}

// TestBroadcastEnvironmentEventsCoalescesABurst pins the settle window.
// Joining a network produces the link coming up and then an address being
// assigned, moments apart, and those mean one thing.
func TestBroadcastEnvironmentEventsCoalescesABurst(t *testing.T) {
	triggers := testTriggers("/notes")
	events := startBroadcaster(t, triggers, 50*time.Millisecond, time.Hour)

	for range 5 {
		events <- TriggerNetwork
	}

	waitFor(t, 2*time.Second, func() bool { return pending(triggers) == 1 })
	drain(triggers)
	time.Sleep(150 * time.Millisecond)
	if fired := drain(triggers); len(fired) != 0 {
		t.Errorf("a second round fired for the same burst: %v", fired)
	}
}

// TestBroadcastEnvironmentEventsRationsRounds pins the cooldown, the backstop
// against an event source nobody anticipated driving the sync loops flat out.
func TestBroadcastEnvironmentEventsRationsRounds(t *testing.T) {
	triggers := testTriggers("/notes")
	events := startBroadcaster(t, triggers, 10*time.Millisecond, time.Hour)

	events <- TriggerNetwork
	waitFor(t, 2*time.Second, func() bool { return pending(triggers) == 1 })
	drain(triggers)

	events <- TriggerNetwork
	time.Sleep(150 * time.Millisecond)
	if fired := drain(triggers); len(fired) != 0 {
		t.Errorf("an event during the cooldown started another round: %v", fired)
	}
}

// TestBroadcastEnvironmentEventsDelaysRatherThanDropsARound is the toggling
// Wi-Fi case. Losing the network and finding it again are two rounds' worth
// of events seconds apart; the first fires while the machine is offline and
// achieves nothing, so the second is the one that has to survive.
func TestBroadcastEnvironmentEventsDelaysRatherThanDropsARound(t *testing.T) {
	triggers := testTriggers("/notes")
	events := startBroadcaster(t, triggers, 10*time.Millisecond, 100*time.Millisecond)

	events <- TriggerNetwork // the link went away
	waitFor(t, 2*time.Second, func() bool { return pending(triggers) == 1 })
	drain(triggers)

	events <- TriggerNetwork // and came back, well inside the cooldown
	waitFor(t, 2*time.Second, func() bool { return pending(triggers) == 1 })
	if fired := drain(triggers); fired["/notes"] != TriggerNetwork {
		t.Errorf("fired = %v, want the reconnect to have been held and then delivered", fired)
	}
}

// TestRepoTriggerLetsAManualRequestDisplaceAnEnvironmentOne covers the one
// case where coalescing two requests into one would lose something. The loop
// defers an environment event to a debounce in progress and does not defer a
// person's request, so a `gitloop sync` collapsed into a waiting wake event
// would be reported as accepted and then wait out the settle window it was
// meant to pre-empt.
func TestRepoTriggerLetsAManualRequestDisplaceAnEnvironmentOne(t *testing.T) {
	trigger := newRepoTrigger()
	trigger.fire(TriggerWake)
	trigger.fire(TriggerManual)

	if got := trigger.take(); got != TriggerManual {
		t.Errorf("take() = %q, want %q", got, TriggerManual)
	}
}

// TestRepoTriggerKeepsAManualRequestBehindAnEnvironmentOne is the other
// direction: an environment event has nothing to add to a request already
// waiting, so it must not downgrade one.
func TestRepoTriggerKeepsAManualRequestBehindAnEnvironmentOne(t *testing.T) {
	trigger := newRepoTrigger()
	trigger.fire(TriggerManual)
	trigger.fire(TriggerWake)

	if got := trigger.take(); got != TriggerManual {
		t.Errorf("take() = %q, want %q", got, TriggerManual)
	}
}

func TestTriggerReasonIsEnvironmental(t *testing.T) {
	cases := map[triggerReason]bool{
		TriggerWake:    true,
		TriggerNetwork: true,
		TriggerManual:  false,
	}
	for reason, want := range cases {
		if got := reason.isEnvironmental(); got != want {
			t.Errorf("%q.isEnvironmental() = %v, want %v", reason, got, want)
		}
	}
}

// TestWatchWakeIgnoresOrdinaryTicks pins the detector's quiet side: probes
// that arrive on schedule are not a suspend, and a daemon that fired on every
// one of them would sync every few seconds forever.
func TestWatchWakeIgnoresOrdinaryTicks(t *testing.T) {
	const probe = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*probe)
	defer cancel()

	woke := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchWake(ctx, probe, 30*time.Second, func() {
			select {
			case woke <- struct{}{}:
			default:
			}
		})
	}()

	<-done
	select {
	case <-woke:
		t.Error("watchWake reported a wake while the machine was plainly awake")
	default:
	}
}

// TestWatchWakeReportsAnUnaccountedGap drives the detector's positive side
// without suspending the machine: a threshold of zero makes any probe that
// takes longer than its interval — which the very first one does — look like
// the gap a suspend leaves behind.
func TestWatchWakeReportsAnUnaccountedGap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	woke := make(chan struct{}, 1)
	go watchWake(ctx, time.Millisecond, 0, func() {
		select {
		case woke <- struct{}{}:
		default:
		}
	})

	select {
	case <-woke:
	case <-time.After(2 * time.Second):
		t.Fatal("watchWake never reported a gap")
	}
}

// TestWakeGapThresholdExceedsProbeInterval guards the arithmetic in
// watchWake: a threshold at or below the probe interval would make every
// slightly late tick look like a suspend.
func TestWakeGapThresholdExceedsProbeInterval(t *testing.T) {
	if wakeGapThreshold <= wakeProbeInterval {
		t.Errorf("wakeGapThreshold (%s) must be comfortably above wakeProbeInterval (%s)",
			wakeGapThreshold, wakeProbeInterval)
	}
}
