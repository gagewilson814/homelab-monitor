package alert

import "testing"

func TestTrackerConfirmed(t *testing.T) {
	tr := NewTracker(1)

	if _, _, ok := tr.Confirmed("agent1"); ok {
		t.Fatal("Confirmed on an unobserved address should report ok=false")
	}

	tr.Observe("agent1", true)
	online, since1, ok := tr.Confirmed("agent1")
	if !ok || !online {
		t.Fatalf("Confirmed after baseline sighting = (%v, %v), want (true, true)", online, ok)
	}
	if since1.IsZero() {
		t.Fatal("Confirmed should report a non-zero since time")
	}

	tr.Observe("agent1", false)
	online, since2, ok := tr.Confirmed("agent1")
	if !ok || online {
		t.Fatalf("Confirmed after offline transition = (%v, %v), want (false, true)", online, ok)
	}
	if !since2.After(since1) {
		t.Fatalf("since should advance on a new confirmed transition: %v -> %v", since1, since2)
	}
}

func TestTracker(t *testing.T) {
	tr := NewTracker(2)

	cases := []struct {
		online bool
		want   string
	}{
		{true, ""},         // first sighting: baseline only, never a transition
		{false, ""},        // 1st offline poll: still pending
		{false, "offline"}, // 2nd consecutive offline poll: confirmed
		{false, ""},        // steady state: no repeat notification
		{true, ""},         // 1st online poll: still pending
		{false, ""},        // blip back to offline resets the pending streak
		{true, ""},         // 1st online poll again
		{true, "online"},   // 2nd consecutive online poll: confirmed
	}

	for i, c := range cases {
		got := tr.Observe("agent1", c.online)
		if got != c.want {
			t.Fatalf("step %d: Observe(%v) = %q, want %q", i, c.online, got, c.want)
		}
	}
}

func TestTrackerForgetClearsAddressAndItsScopedKeys(t *testing.T) {
	tr := NewTracker(1)

	// Host state plus the per-service and per-metric keys scoped under it,
	// which is how the backend names them.
	tr.Observe("agent1", false)
	tr.Observe("agent1/plex", false)
	tr.Observe("agent1/cpu", false)
	tr.Observe("agent2", false)

	tr.Forget("agent1")

	for _, key := range []string{"agent1", "agent1/plex", "agent1/cpu"} {
		if _, _, ok := tr.Confirmed(key); ok {
			t.Errorf("Confirmed(%q) still present after Forget", key)
		}
	}
	// An unrelated agent must be untouched, and re-observing a forgotten
	// address must start a fresh baseline (no transition reported).
	if _, _, ok := tr.Confirmed("agent2"); !ok {
		t.Error("Forget(agent1) should not have touched agent2")
	}
	if got := tr.Observe("agent1", true); got != "" {
		t.Errorf("re-observing a forgotten address = %q, want a fresh baseline", got)
	}
}

func TestTrackerIndependentAddresses(t *testing.T) {
	tr := NewTracker(1)

	if got := tr.Observe("a", true); got != "" {
		t.Fatalf("a first sighting: got %q, want \"\"", got)
	}
	if got := tr.Observe("b", false); got != "" {
		t.Fatalf("b first sighting: got %q, want \"\"", got)
	}
	if got := tr.Observe("a", false); got != "offline" {
		t.Fatalf("a going offline: got %q, want offline", got)
	}
	if got := tr.Observe("b", false); got != "" {
		t.Fatalf("b unchanged: got %q, want \"\"", got)
	}
}
