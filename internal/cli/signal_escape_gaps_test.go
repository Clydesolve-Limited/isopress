package cli

// Signal-guard gap tests (verification round for defect B of
// 2026-10-19, review round 2). signal_escape_test.go pins the core
// ordering, the real-signal child kill, and the settle-poll leak
// checks; this file closes the audit gaps around the guard's corners,
// re-targeted to the single-channel consumer that replaced the
// two-channel drain:
//
//   - THIRD signal / signal AFTER the escape fired: with an injected
//     force that RETURNS (production never returns - the process dies),
//     the goroutine must retire after one force call, exactly one
//     forcing-exit line, no panic and no re-fire on further signals.
//   - FIFO ORDERING on the single channel: with both signals of a pair
//     queued before the guard can run (or the guard consuming the first
//     between the two sends - racy but EQUIVALENT), the guard always
//     consumes the OLDEST signal as the graceful trigger and the NEXT
//     one as the escape trigger - pinned for every SIGINT/SIGTERM
//     pairing, including the pure doubles. This replaces the obsolete
//     one-copy-drain tests: there is no drain and no second channel, so
//     ordering is the whole determinism story.
//   - FAST-SHUTDOWN window: signals arriving strictly after done has
//     closed and the goroutine retired are inert (no force, no line, no
//     panic), and a done-close racing a second signal can fire the
//     force AT MOST once per run - never double-reset, never panic.
//     LOOPED per the review (B1 was intermittent): a few hundred fresh
//     goroutine runs per invocation.
//   - a stray signal copy in a run whose Execute is finishing (no first
//     signal ever consumed as such) never fires the escape: the force
//     path is only reachable after the first signal was consumed from
//     the channel.
//
// Stdlib only; no sleeps in hot paths - channel sync, the injected
// recorder, and the settle-then-poll leak pattern carry all ordering.

import (
	"bytes"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSecondSignalEscapeForceFiresExactlyOnceAndGoroutineRetires pins
// the THIRD-signal corner. Production force (resetAndReraise) never
// returns - the kernel kills the process - but the injectable core must
// stay safe when the injected force DOES return: exactly one
// forcing-exit line, the goroutine retires WITHOUT waiting on done (the
// escape path itself leaks nothing), and every further signal on the
// (already stopped) channel is inert - no re-fire, no double-reset, no
// panic.
func TestSecondSignalEscapeForceFiresExactlyOnceAndGoroutineRetires(t *testing.T) {
	baseline := settleEscapeGoroutines(t)
	sigs := make(chan os.Signal, 4)
	done := make(chan struct{}) // deliberately NOT closed until after the settle
	cancelled := make(chan struct{}, 1)
	forced := make(chan os.Signal, 4)
	var errBuf bytes.Buffer

	startSecondSignalEscape(sigs, func() { cancelled <- struct{}{} }, done, &errBuf, func(s os.Signal) { forced <- s })

	sigs <- syscall.SIGTERM // the FIRST signal: graceful path only
	waitGuardCancel(t, cancelled)
	sigs <- syscall.SIGINT // the genuine SECOND signal: fires the escape

	select {
	case got := <-forced:
		if got != syscall.SIGINT {
			t.Fatalf("forced signal = %v, want SIGINT", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second signal did not fire the force action")
	}

	// THIRD signal (and a fourth), delivered after the escape fired.
	// The injected force returned, so the goroutine has retired; these
	// sends must be inert.
	sigs <- syscall.SIGINT
	sigs <- syscall.SIGTERM
	select {
	case got := <-forced:
		t.Fatalf("force fired AGAIN (%v) after the escape already fired; exactly once per run", got)
	case <-time.After(50 * time.Millisecond):
		// Expected: negative-observation bound, not a sleep race - the
		// positive assertion above drove the sequencing.
	}

	if lines := strings.Count(errBuf.String(), "second signal received, forcing exit"); lines != 1 {
		t.Errorf("forcing-exit lines = %d, want exactly 1: %q", lines, errBuf.String())
	}

	// The escape path retires on its own (production dies in force; the
	// injectable core simply returns) - no done-close needed, no leak.
	waitEscapeGoroutinesBaseline(t, baseline)
	close(done) // tidy: the retired goroutine ignores it
}

// TestSecondSignalEscapeFIFOOrderingOnPairedSignals pins the
// single-channel determinism claim that replaced the defect-B drain:
// BOTH signals of a pair are queued back-to-back before any wait. The
// guard may consume the first between the two sends or after both - the
// interleaving is racy but EQUIVALENT, because channel FIFO always hands
// it the OLDEST signal first: the oldest is the graceful trigger
// (cancel), the next fires the escape (force). First-vs-second is
// ORDER, not a race between two channels' copies. Every SIGINT/SIGTERM
// pairing is pinned, including the pure doubles (the escape must work
// for SIGTERM alone, no SIGINT involved).
func TestSecondSignalEscapeFIFOOrderingOnPairedSignals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  syscall.Signal // the FIRST signal (graceful trigger)
		second syscall.Signal // the genuine SECOND signal (escape trigger)
	}{
		{"SIGINT first then SIGTERM", syscall.SIGINT, syscall.SIGTERM},
		{"SIGTERM first then SIGINT", syscall.SIGTERM, syscall.SIGINT},
		{"SIGTERM first then SIGTERM (pure SIGTERM double)", syscall.SIGTERM, syscall.SIGTERM},
		{"SIGINT first then SIGINT (pure SIGINT double)", syscall.SIGINT, syscall.SIGINT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sigs := make(chan os.Signal, 4)
			done := make(chan struct{})
			defer close(done)
			cancelled := make(chan struct{}, 1)
			forced := make(chan os.Signal, 4)
			var errBuf bytes.Buffer

			startSecondSignalEscape(sigs, func() { cancelled <- struct{}{} }, done, &errBuf, func(s os.Signal) { forced <- s })

			sigs <- tc.first
			sigs <- tc.second

			waitGuardCancel(t, cancelled) // the first signal became the graceful trigger
			select {
			case got := <-forced:
				if got != tc.second {
					t.Fatalf("forced signal = %v, want the SECOND signal %v: FIFO must consume the oldest signal first", got, tc.second)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the queued second signal never fired the force action")
			}

			// Exactly once: nothing else was queued, so no re-fire.
			select {
			case got := <-forced:
				t.Fatalf("force fired again (%v); exactly once per pair", got)
			case <-time.After(50 * time.Millisecond):
				// Expected: negative-observation bound.
			}
			if lines := strings.Count(errBuf.String(), "second signal received, forcing exit"); lines != 1 {
				t.Errorf("forcing-exit lines = %d, want exactly 1: %q", lines, errBuf.String())
			}
		})
	}
}

// TestSecondSignalEscapeInertAfterDoneClosed pins the fast-shutdown
// window's deterministic half: the first signal began graceful
// shutdown, then done closed (Execute's exit defer) BEFORE any second
// signal - the escape goroutine retires, and any signal still delivered
// to the channel afterwards is inert: no force action, no forcing-exit
// line, no panic, no leak. This is the "signal arrives after Execute is
// basically done" slice.
func TestSecondSignalEscapeInertAfterDoneClosed(t *testing.T) {
	// Warm up the os/signal package first (see the same note in
	// TestSecondSignalEscapeNoLeakOnCleanRun): its process-wide delivery
	// goroutine starts on the first in-process Notify call.
	warm := make(chan os.Signal, 1)
	signal.Notify(warm, syscall.SIGTERM)
	signal.Stop(warm)

	baseline := settleEscapeGoroutines(t)
	sigs := make(chan os.Signal, 4)
	done := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	forced := make(chan os.Signal, 4)
	var errBuf bytes.Buffer

	startSecondSignalEscape(sigs, func() { cancelled <- struct{}{} }, done, &errBuf, func(s os.Signal) { forced <- s })

	sigs <- syscall.SIGTERM // first signal: graceful shutdown begins
	waitGuardCancel(t, cancelled)
	close(done) // ...and shutdown finished before any second signal: Execute is exiting
	waitEscapeGoroutinesBaseline(t, baseline)

	// Every post-retirement signal is inert. Non-blocking sends: the
	// goroutine has retired (nobody drains the channel), so a full
	// buffer must not block the test - inertness is about the ABSENCE
	// of an effect, not about queueing.
	for i := 0; i < 5; i++ {
		select {
		case sigs <- syscall.SIGINT:
		default:
		}
		select {
		case sigs <- syscall.SIGTERM:
		default:
		}
	}
	select {
	case got := <-forced:
		t.Fatalf("force fired (%v) after done closed and the goroutine retired; post-shutdown signals must be inert", got)
	case <-time.After(50 * time.Millisecond):
		// Expected: negative-observation bound.
	}
	if out := errBuf.String(); strings.Contains(out, "forcing exit") {
		t.Errorf("post-retirement signals printed the forcing-exit line: %q", out)
	}
}

// TestSecondSignalEscapeStraySignalNeverFiresForce pins the guard's
// clean-run discipline: a stray signal sitting in the channel of a run
// whose Execute is finishing (in production: the signal landed between
// execute's return and the exit defers) can NEVER fire the escape - the
// force path is only reachable after a first signal was consumed from
// the channel, and here done closes in the same instant. Honest
// single-channel semantics: the phase-1 select may legally consume the
// stray and call the injected cancel before it sees done (both cases
// are ready; the runtime picks) - in production that cancel is an
// idempotent no-op on an already-finished run - but the force never
// fires and the goroutine always retires.
func TestSecondSignalEscapeStraySignalNeverFiresForce(t *testing.T) {
	baseline := settleEscapeGoroutines(t)
	sigs := make(chan os.Signal, 4)
	done := make(chan struct{})
	forced := make(chan os.Signal, 4)
	var errBuf bytes.Buffer

	startSecondSignalEscape(sigs, func() {}, done, &errBuf, func(s os.Signal) { forced <- s })

	sigs <- syscall.SIGINT // stray signal; the run has no first-signal intent
	close(done)            // clean Execute exit races the stray (either choice is legal)
	waitEscapeGoroutinesBaseline(t, baseline)

	select {
	case got := <-forced:
		t.Fatalf("force fired (%v) in a clean run; the escape must only act on a genuine second signal", got)
	case <-time.After(50 * time.Millisecond):
		// Expected: negative-observation bound.
	}
	if out := errBuf.String(); strings.Contains(out, "forcing exit") {
		t.Errorf("stray signal printed the forcing-exit line in a clean run: %q", out)
	}
}

// raceWindowIterations is the loop depth of the done-close-vs-second-
// signal race below: B1 was an INTERMITTENT race (5 of 2000 iterations
// under -race), so the reviewer demanded the racy window be hammered
// with fresh goroutines each pass, not sampled once per invocation.
const raceWindowIterations = 200

// TestSecondSignalEscapeDoneCloseRacesSecondSignalBounded hammers the
// inherently racy window - the second signal arrives in the same instant
// done closes (a signal landing between execute's return and the exit
// defer) - in a LOOP of fresh goroutines (see raceWindowIterations).
// Every interleaving is legal for the core: the force may fire (the
// user DID signal during shutdown) or not (the guard saw done first),
// but it must fire AT MOST ONCE per run, never panic, and every
// goroutine must retire. The per-run bound is asserted once at the end
// (each run has its own recorder; draining them also establishes the
// happens-before edges for the forcing-line buffer reads), and the
// line/fire consistency pins one line per fire.
func TestSecondSignalEscapeDoneCloseRacesSecondSignalBounded(t *testing.T) {
	baseline := settleEscapeGoroutines(t)

	type run struct {
		forced chan os.Signal
		errBuf *bytes.Buffer
	}
	runs := make([]run, 0, raceWindowIterations)
	for i := 0; i < raceWindowIterations; i++ {
		sigs := make(chan os.Signal, 4)
		done := make(chan struct{})
		cancelled := make(chan struct{}, 1)
		r := run{forced: make(chan os.Signal, 4), errBuf: &bytes.Buffer{}}
		startSecondSignalEscape(sigs, func() { cancelled <- struct{}{} }, done, r.errBuf, func(s os.Signal) { r.forced <- s })

		sigs <- syscall.SIGTERM // first signal: graceful shutdown begins
		waitGuardCancel(t, cancelled)
		// Alternate the racy order: the second signal can land just
		// before or just after the done-close (Execute's exit defer) -
		// both orders are inside the window defect B lived in, and
		// every interleaving must stay at-most-once per run.
		if i%2 == 0 {
			close(done) // Execute finished in the same instant...
			sigs <- syscall.SIGINT
		} else {
			sigs <- syscall.SIGINT // ...and the second signal races the done-close
			close(done)
		}
		runs = append(runs, r)
	}

	// The goroutines always retire (every run closed done): one leak
	// check for the whole loop.
	waitEscapeGoroutinesBaseline(t, baseline)

	// Per-run AT MOST ONE force: a run with two entries double-handled
	// the racing window. Non-blocking receive (the channels are never
	// closed - each run's guard already retired).
	totalForced := 0
	for i, r := range runs {
		n := 0
		for {
			select {
			case <-r.forced:
				n++
				continue
			default:
			}
			break
		}
		if n > 1 {
			t.Errorf("run %d: force fired %d times in the racy window, want at most 1", i, n)
		}
		totalForced += n
	}

	// One forcing-exit line per fire - never a line without a fire,
	// never a double line per fire.
	totalLines := 0
	for _, r := range runs {
		totalLines += strings.Count(r.errBuf.String(), "second signal received, forcing exit")
	}
	if totalLines != totalForced {
		t.Errorf("forcing-exit lines = %d across %d runs, want exactly one per fire (%d fires observed)",
			totalLines, raceWindowIterations, totalForced)
	}
	t.Logf("%d/%d racy-window iterations fired the force (legal either way)", totalForced, raceWindowIterations)
}
