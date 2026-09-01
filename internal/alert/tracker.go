// Package alert debounces per-agent online/offline transitions so a single
// dropped poll doesn't trigger a notification.
package alert

import "sync"

type agentState struct {
	confirmedOnline bool
	pendingOnline   bool
	pendingCount    int
}

// Tracker holds the last confirmed state and any in-progress transition for
// each agent address. It requires the same result to repeat `threshold`
// times in a row before it reports a transition, filtering out one-off
// network blips.
type Tracker struct {
	threshold int

	mu    sync.Mutex
	state map[string]agentState
}

func NewTracker(threshold int) *Tracker {
	if threshold < 1 {
		threshold = 1
	}
	return &Tracker{threshold: threshold, state: make(map[string]agentState)}
}

// Observe records one poll result for an address. It returns "online" or
// "offline" the moment that result crosses the debounce threshold and
// becomes the newly confirmed state, or "" if there's nothing to report
// (steady state, still pending, or the agent's first-ever observation,
// which only establishes a baseline).
func (t *Tracker) Observe(address string, online bool) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	s, exists := t.state[address]
	if !exists {
		t.state[address] = agentState{confirmedOnline: online}
		return ""
	}

	if online == s.confirmedOnline {
		s.pendingCount = 0
		t.state[address] = s
		return ""
	}

	if online == s.pendingOnline {
		s.pendingCount++
	} else {
		s.pendingOnline = online
		s.pendingCount = 1
	}

	if s.pendingCount < t.threshold {
		t.state[address] = s
		return ""
	}

	s.confirmedOnline = online
	s.pendingCount = 0
	t.state[address] = s

	if online {
		return "online"
	}
	return "offline"
}
