package daemon

import (
	"context"
	"log/slog"
	"time"
)

const (
	// wakeProbeInterval is how often the wake detector looks for a gap. It
	// also bounds how long after a wake the gap is noticed, because the
	// timer itself does not advance while the machine is asleep.
	wakeProbeInterval = 5 * time.Second
	// wakeGapThreshold is how much unaccounted wall-clock time has to pass
	// between two probes before it is read as a suspend. Comfortably above
	// any scheduling delay, and above the clock steps an NTP correction
	// makes.
	wakeGapThreshold = 30 * time.Second

	// environmentSettle collects a burst of environment events into one
	// trigger. Joining a network produces several in a row — the link comes
	// up, then an address is assigned — and they mean one thing.
	environmentSettle = 2 * time.Second
	// environmentCooldown is the floor between two broadcasts, so that no
	// unforeseen source of environment events can drive the sync loops
	// faster than this.
	environmentCooldown = 15 * time.Second
)

// watchWake calls onWake when the machine appears to have been asleep.
//
// macOS has no wake notification a Go program can subscribe to without cgo,
// but it does not need one: the monotonic clock a Go timer runs on stops
// while the machine is suspended, and the wall clock does not. Two probes
// 5 seconds of monotonic time apart that are half an hour apart on the wall
// clock is a suspend, and nothing else.
//
// A clock step large enough to clear the threshold — an NTP correction after
// a dead battery, a manual change — reads as a wake too. That costs one extra
// sync cycle, which is the cheapest possible way to be wrong here.
func watchWake(ctx context.Context, probeInterval, gapThreshold time.Duration, onWake func()) {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	previous := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			// Round(0) strips the monotonic reading from both times, which is
			// what makes the subtraction below wall-clock arithmetic.
			elapsed := now.Round(0).Sub(previous.Round(0))
			previous = now
			if elapsed-probeInterval > gapThreshold {
				onWake()
			}
		}
	}
}

// runEnvironmentWatcher watches for the two things that change a laptop's
// view of the world without changing a single file: waking from sleep, and
// the network coming back. Both mean the remote may have moved on while this
// machine could not see it, and both are the moment a user notices gitloop is
// behind — the lid opens and the notes are stale.
func runEnvironmentWatcher(ctx context.Context, triggers map[string]*repoTrigger, logger *slog.Logger) {
	events := make(chan triggerReason, 1)
	notify := func(reason triggerReason) {
		select {
		case events <- reason:
		default:
		}
	}

	go watchWake(ctx, wakeProbeInterval, wakeGapThreshold, func() { notify(TriggerWake) })
	go func() {
		if err := watchNetworkChanges(ctx, func() { notify(TriggerNetwork) }, logger); err != nil {
			// Losing this watcher costs responsiveness, not correctness: the
			// fetch interval still runs. Reported rather than retried, since
			// the failures it can have are all permanent for the process.
			logger.Error("network change detection stopped", "error", err)
		}
	}()

	broadcastEnvironmentEvents(ctx, events, triggers, environmentSettle, environmentCooldown, logger)
}

// broadcastEnvironmentEvents turns a stream of environment events into
// rounds of sync requests. Every repository is told at once, since neither
// waking nor reconnecting is about any one of them.
//
// Events are collected over settle and then rationed by cooldown. Joining a
// network produces several in a row — the link comes up, then an address is
// assigned — and they mean one thing; the cooldown then bounds how fast any
// unforeseen source of events could drive the sync loops.
//
// The cooldown delays a round rather than dropping it. Toggling Wi-Fi off and
// back on produces two rounds' worth of events seconds apart: the first fires
// while the machine is offline and achieves nothing, so discarding the second
// would leave the reconnect — the one that could actually sync — unheard until
// the next interval tick.
func broadcastEnvironmentEvents(
	ctx context.Context,
	events <-chan triggerReason,
	triggers map[string]*repoTrigger,
	settleFor, cooldown time.Duration,
	logger *slog.Logger,
) {
	var (
		settle    *time.Timer
		settleC   <-chan time.Time
		pending   triggerReason
		lastBurst time.Time
	)
	defer func() {
		if settle != nil {
			settle.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case reason := <-events:
			// The latest reason wins: it describes the state the machine has
			// arrived at, and the earlier ones describe how it got there.
			pending = reason
			if settle == nil {
				settle = time.NewTimer(max(settleFor, time.Until(lastBurst.Add(cooldown))))
				settleC = settle.C
			}

		case <-settleC:
			settle, settleC = nil, nil
			lastBurst = time.Now()
			logger.Info("environment changed, syncing every repository", "trigger", pending, "repositories", len(triggers))
			for _, trigger := range triggers {
				trigger.fire(pending)
			}
		}
	}
}
