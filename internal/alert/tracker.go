// Package alert debounces per-agent online/offline transitions so a single
// dropped poll doesn't trigger a notification.
package alert

import (
	"strings"
	"sync"
	"time"
)

type agentState struct {
	confirmedOnline bool
	confirmedAt     time.Time
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
		t.state[address] = agentState{confirmedOnline: online, confirmedAt: time.Now()}
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
	s.confirmedAt = time.Now()
	s.pendingCount = 0
	t.state[address] = s

	if online {
		return "online"
	}
	return "offline"
}

// Forget drops all debounce state for address (and, via the prefix form
// used for per-service and per-metric keys, everything scoped under it), so
// an agent that is removed and later re-added starts from a clean baseline
// instead of inheriting the state it had when it left.
func (t *Tracker) Forget(address string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.state, address)
	for key := range t.state {
		if strings.HasPrefix(key, address+"/") {
			delete(t.state, key)
		}
	}
}

// Confirmed returns the last confirmed state for address and the time it
// became that state. ok is false if address has never been observed. This
// lets a caller build a snapshot of every currently-bad key (confirmed
// online == false) without waiting for the exact poll where the transition
// happened - useful for an aggregated "what's wrong right now" view, as
// opposed to Observe's one-shot transition notifications.
func (t *Tracker) Confirmed(address string) (online bool, since time.Time, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s, exists := t.state[address]
	if !exists {
		return false, time.Time{}, false
	}
	return s.confirmedOnline, s.confirmedAt, true
}
