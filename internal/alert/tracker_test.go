package alert

import "testing"

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
