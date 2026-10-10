package tracing

import "testing"

// Every machine that touches a run has to arrive at the same trace for it,
// with nothing to go on but the run's id.
func TestARunsTraceComesFromItsID(t *testing.T) {
	a, again, other := Ask("run-1"), Ask("run-1"), Ask("run-2")
	if a.Parent == "" || a.Parent != again.Parent {
		t.Errorf("the same run gave %q and then %q", a.Parent, again.Parent)
	}
	if a.Parent == other.Parent {
		t.Errorf("two runs share %q", a.Parent)
	}
	if a.Asked.IsZero() {
		t.Error("a step asked for has no time it was asked at")
	}
}
