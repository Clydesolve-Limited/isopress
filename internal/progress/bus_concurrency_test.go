package progress

// T1.1 hardening tests for the Bus concurrency and lifecycle contracts
// (DESIGN_SPEC 9.1: "a buffered channel consumed by EXACTLY ONE renderer
// goroutine"). The hash worker pool (T1.4) will Emit from up to 8
// goroutines concurrently and the service coordinator will Finish while
// workers may still be trickling events, so the properties pinned here
// are the ones that keep that pattern race-free and leak-free:
//
//   - concurrent Emit from many producers is race-free and the
//     Dropped() accounting is exact (received + dropped == emitted);
//   - Finish/Emit(Done) is safe while producers are still emitting;
//   - Emit after the renderer has exited is safe (no panic, no hang,
//     overflow keeps counting into the same cumulative counter);
//   - the documented single-Finish contract: a second Done buffers
//     without blocking while the channel has room, and Finish blocks
//     forever once the channel is full with no renderer consuming
//     (callers MUST start a renderer first - event.go contract);
//   - Dropped is cumulative per Bus and is never reset by Finish;
//   - a second CONCURRENT renderer Run on the same Bus panics (the
//     exactly-one-consumer guard), while a sequential re-run after the
//     first renderer returned is allowed and picks up stale buffered
//     events (the hazard that makes the Bus single-operation).

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingRenderer is a test renderer that counts every event the
// renderer goroutine actually consumes. It honors the one-consumer
// guard exactly like the production renderers.
type countingRenderer struct{ consumed atomic.Int64 }

func (c *countingRenderer) Run(bus *Bus) Done {
	acquireBus(bus)
	defer releaseBus(bus)
	for {
		ev := <-bus.ch
		c.consumed.Add(1)
		if d, ok := ev.(Done); ok {
			return d
		}
	}
}

// TestConcurrentEmitFromMultipleProducersExactAccounting: 8 goroutines
// (standing in for the T1.4 hash worker pool) Emit 32,000 events at a
// draining renderer. Under -race this exercises the Emit path for data
// races; the accounting assertion is exact and scheduling-independent:
// every emitted event is either consumed by the renderer or counted by
// Dropped(), never lost and never double-counted.
func TestConcurrentEmitFromMultipleProducersExactAccounting(t *testing.T) {
	const producers = 8
	const perProducer = 4000

	bus := NewBus()
	c := &countingRenderer{}
	wait := Start(c, bus)

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				bus.Emit(ItemDone{Bytes: 1, Path: "some/file"})
			}
		}()
	}
	wg.Wait()
	bus.Emit(Done{Summary: "pool done"}) // guaranteed path after producers joined
	final := wait()

	if final.Summary != "pool done" {
		t.Errorf("Run returned %+v, want the terminal Done verbatim", final)
	}
	emitted := int64(producers*perProducer) + 1 // + the Done event
	consumed := c.consumed.Load()
	dropped := bus.Dropped()
	if consumed+dropped != emitted {
		t.Errorf("accounting mismatch: consumed(%d) + dropped(%d) = %d, want %d (every event consumed or counted, never lost)",
			consumed, dropped, consumed+dropped, emitted)
	}
	if consumed < 1 {
		t.Errorf("renderer consumed %d events, want at least the terminal Done", consumed)
	}
}

// TestFinishConcurrentWithInflightEmits pins the real service pattern
// of T1.4/T1.6: producers keep Emitting (Ticked heartbeats) while the
// coordinator Emits Done on another goroutine. The guaranteed-delivery
// path must be race-free against concurrent incremental Emits, the
// renderer must return the terminal Done, and the summary must print
// exactly once.
func TestFinishConcurrentWithInflightEmits(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModePlain, Stderr: w, Now: newFakeClock().Now}), bus)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					bus.Emit(Tick{})
				}
			}
		}()
	}

	// Let the producers hammer the bus, then finish while they are
	// still emitting: Done must win through the guaranteed path and
	// must not race with the incremental Emits (-race).
	time.Sleep(10 * time.Millisecond)
	bus.Emit(Done{Summary: "finish under fire"})
	final := wait()

	close(stop)
	wg.Wait()

	if final.Summary != "finish under fire" {
		t.Errorf("Run returned %+v, want the terminal Done verbatim", final)
	}
	if n := strings.Count(w.String(), "finish under fire"); n != 1 {
		t.Errorf("summary printed %d times under concurrent emits, want exactly 1:\n%q", n, w.String())
	}
}

// TestEmitAfterFinishIsSafe pins the documented emit-after-Finish
// behavior for the incremental (non-Done) event kinds: the renderer
// goroutine is gone, so events fill the empty buffer first and every
// further event is dropped into the SAME cumulative counter - no
// panic, no block, and the already-printed summary is untouched. This
// is the behavior that makes a service's late trickle events (workers
// finishing after the coordinator called Finish) harmless.
func TestEmitAfterFinishIsSafe(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModePlain, Stderr: w, Now: newFakeClock().Now}), bus)
	bus.Emit(Phase{Name: "copy"})
	bus.Emit(Done{Summary: "done once"})
	wait()

	before := bus.Dropped()
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		// 4 kinds x (ChannelBuffer + 64): the first ChannelBuffer
		// events fill the now-empty buffer, the rest must drop.
		for i := 0; i < ChannelBuffer+64; i++ {
			bus.Emit(Tick{})
			bus.Emit(Warn{Msg: "late warning"})
			bus.Emit(Error{Msg: "late error"})
			bus.Emit(ItemDone{Bytes: 1})
		}
	}()
	select {
	case <-produced:
	case <-time.After(30 * time.Second):
		t.Fatal("Emit after Finish blocked the producer; incremental events must never block")
	}

	wantDropped := before + 3*ChannelBuffer + 256 // 4*(ChannelBuffer+64) - ChannelBuffer buffered
	if got := bus.Dropped(); got != wantDropped {
		t.Errorf("Dropped() = %d after post-Finish overflow, want %d (counter keeps accumulating; nothing resets it)",
			got, wantDropped)
	}
	if n := strings.Count(w.String(), "done once"); n != 1 {
		t.Errorf("summary printed %d times after the post-Finish flood, want exactly 1:\n%q", n, w.String())
	}
}

// TestSecondDoneBuffersWithoutBlocking documents the single-Finish
// contract (event.go: "Exactly one Finish per Bus is the intended
// usage"): a second Done, with channel room and no consumer, is
// accepted into the buffer without blocking or panicking. It only
// blocks once the buffer is full - the case pinned by
// TestFinishWithoutConsumerBlocksWhenFull.
func TestSecondDoneBuffersWithoutBlocking(t *testing.T) {
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeSilent}), bus)
	bus.Emit(Done{Summary: "first"})
	wait()

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		bus.Emit(Done{Summary: "second"})
	}()
	select {
	case <-secondDone:
	case <-time.After(10 * time.Second):
		t.Fatal("second Done blocked on an empty channel; documented behavior is to buffer while there is room")
	}
}

// TestFinishWithoutConsumerBlocksWhenFull pins the documented contract
// that callers MUST start a renderer before Finish (event.go): with no
// consumer and a completely full channel, Finish blocks until a
// consumer drains the bus. The test first proves the blockage, then
// drains the channel to release the goroutine (no leaked goroutine at
// test end).
func TestFinishWithoutConsumerBlocksWhenFull(t *testing.T) {
	bus := NewBus()
	for i := 0; i < ChannelBuffer; i++ {
		bus.Emit(Tick{})
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		bus.Finish(Done{Summary: "needs a consumer"})
	}()
	select {
	case <-finished:
		t.Fatal("Finish completed with a full channel and no renderer; the documented contract says it must block until consumed")
	case <-time.After(100 * time.Millisecond):
		// As documented: blocked waiting for a consumer.
	}

	// Release the blocked Finish by consuming the bus exactly like a
	// renderer would, and wait for it to complete.
	go func() {
		for {
			if _, ok := (<-bus.ch).(Done); ok {
				return
			}
		}
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("Finish did not complete after a consumer drained the bus")
	}
}

// TestDroppedCounterIsCumulativeNeverReset: Dropped is a per-Bus,
// lifetime-cumulative counter. Neither Finish nor a renderer
// start/stop cycle resets it - asserted here so a future change to the
// reset semantics is a conscious, test-visible one.
func TestDroppedCounterIsCumulativeNeverReset(t *testing.T) {
	bus := NewBus()
	for i := 0; i < ChannelBuffer+7; i++ {
		bus.Emit(Tick{})
	}
	if got := bus.Dropped(); got != 7 {
		t.Fatalf("Dropped() = %d after overflow without consumer, want 7", got)
	}

	wait := Start(New(Options{Mode: ModeSilent}), bus)
	bus.Emit(Done{Summary: "op complete"})
	wait()
	if got := bus.Dropped(); got != 7 {
		t.Errorf("Dropped() = %d after Finish/renderer exit, want 7 (the terminal Done is exempt from dropping and nothing resets the counter)", got)
	}

	// Overflow resumes into the same counter once the renderer is gone.
	for i := 0; i < ChannelBuffer+5; i++ {
		bus.Emit(Tick{})
	}
	if got := bus.Dropped(); got != 12 {
		t.Errorf("Dropped() = %d after a second overflow wave, want 12 (cumulative across the Bus lifetime)", got)
	}
}

// TestBusReuseDeliversStalePostFinishEvents documents WHY the Bus is a
// single-operation object (event.go: "Exactly one Finish per Bus is the
// intended usage"): Dropped never resets AND events emitted after
// Finish stay buffered, so a second renderer run on the same Bus first
// renders operation-1 leftovers. Pinned so that any future
// Bus-reuse/rearm support is a conscious change, not an accident.
func TestBusReuseDeliversStalePostFinishEvents(t *testing.T) {
	bus := NewBus()
	w1 := &lockedWriter{}
	wait1 := Start(New(Options{Mode: ModePlain, Stderr: w1, Now: newFakeClock().Now}), bus)
	bus.Emit(Phase{Name: "scan"})
	bus.Emit(ItemDone{Bytes: 5, Path: "a"}) // one op-1 progress line under the scan prefix
	bus.Emit(Done{Summary: "op1 complete"})
	wait1()

	// A misbehaving caller emits into the "finished" bus; these events
	// sit in the empty buffer because nothing consumes them.
	bus.Emit(Phase{Name: "copy"})
	bus.Emit(ItemDone{Bytes: 7, Path: "stale"})
	// A second renderer on the same Bus is allowed once the first
	// renderer returned (the consumer guard is released), and it will
	// render the stale leftovers first.
	w2 := &lockedWriter{}
	wait2 := Start(New(Options{Mode: ModePlain, Stderr: w2, Now: newFakeClock().Now}), bus)
	bus.Emit(Done{Summary: "op2 complete"})
	wait2()

	if !strings.Contains(w2.String(), "copy: files 1, 7 B") {
		t.Errorf("second renderer run did not render the stale buffered op-1 events; "+
			"the single-operation contract of the Bus has changed - update event.go docs:\n%q", w2.String())
	}
	if !strings.Contains(w2.String(), "op2 complete") {
		t.Errorf("second renderer run missing its own summary:\n%q", w2.String())
	}
	if n := strings.Count(w1.String(), "scan"); n == 0 {
		t.Errorf("first renderer lost its own output during reuse:\n%q", w1.String())
	}
}

// TestSecondConcurrentRendererRunPanics pins the "exactly one renderer
// goroutine" guard of DESIGN_SPEC 9.1: a second Run over a Bus that is
// already being consumed panics instead of silently interleaving two
// renderers onto the output stream. Cleanup releases the first
// renderer so no goroutine leaks.
func TestSecondConcurrentRendererRunPanics(t *testing.T) {
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeSilent}), bus)
	defer func() {
		bus.Finish(Done{})
		wait()
	}()

	// Start launches the first renderer asynchronously; wait until it
	// has actually acquired the bus so the guard fires on the SECOND
	// Run (an unrecovered panic in the first renderer - the Start
	// goroutine - would crash the whole test binary).
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := busConsumers.Load(bus); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first renderer never acquired the bus")
		}
		time.Sleep(time.Millisecond)
	}

	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		// If the guard ever regresses this Run blocks on the channel
		// instead of panicking; the timeout below turns that into a
		// failure rather than a hung test.
		_ = New(Options{Mode: ModeSilent}).Run(bus)
	}()

	select {
	case p := <-panicked:
		if p == nil {
			t.Fatal("second concurrent Run returned normally; DESIGN_SPEC 9.1 requires exactly one consumer per Bus")
		}
		msg, _ := p.(string)
		if !strings.Contains(msg, "second renderer") {
			t.Errorf("panic value = %v, want the explicit one-consumer guard message", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second concurrent Run blocked instead of panicking on the one-consumer guard")
	}
}
