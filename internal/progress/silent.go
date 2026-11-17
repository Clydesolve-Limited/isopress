package progress

// T1.1: the silent renderer. It consumes the full event stream and
// writes ZERO bytes anywhere (DESIGN_SPEC 9.4 lists it among the
// in-tree renderers; the 9.2 selection matrix never picks it
// automatically - it is reachable via ModeSilent and exists for
// embedding and test callers that want the bus lifecycle without any
// output). Run still returns the final Done event, so interruption and
// summary state remain observable.
type silentRenderer struct{}

func (r *silentRenderer) Run(bus *Bus) Done {
	acquireBus(bus)
	defer releaseBus(bus)
	for {
		if done, ok := (<-bus.ch).(Done); ok {
			return done
		}
	}
}
