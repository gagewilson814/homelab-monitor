package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"homeserver-monitor/internal/alert"
	"homeserver-monitor/internal/auth"
	"homeserver-monitor/internal/stats"
)

// fakeAgent is an httptest server that plays the role of a monitored Agent,
// returning a canned /stats payload. It lets the backend tests exercise the
// real poll/aggregate/serve path without a live machine.
type fakeAgent struct {
	server *httptest.Server
	// failNext flips the agent into failure mode for one poll, to test that a
	// bad agent is reported via Err rather than crashing the fleet.
	failNext bool
}

func newFakeAgent(t *testing.T, hostname string) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{}
	fa.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if fa.failNext {
			fa.failNext = false
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		payload := stats.Response{
			Hostname:    hostname,
			CPUUsage:    10.5,
			MemoryUsage: 20.0,
			DiskUsage:   30.0,
			Uptime:      1234,
			Services:    []stats.ServiceStatus{{Name: "plex", Port: 32400, Up: true}},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("fake agent encode: %v", err)
		}
	}))
	t.Cleanup(fa.server.Close)
	return fa
}

func (fa *fakeAgent) address() string {
	u, err := url.Parse(fa.server.URL)
	if err != nil {
		// Unreachable in practice (httptest always yields a valid URL), but
		// fail loudly rather than returning a bogus address mid-test.
		panic("invalid fake agent url: " + err.Error())
	}
	return u.Host
}

// recordingNotifier is a Notifier test double that captures every message
// sendAlert dispatches instead of hitting a real webhook. Send pushes the
// message onto a buffered channel; withRecordingNotifier drains it after fn()
// returns. Because sendAlert dispatches on a fire-and-forget goroutine with no
// completion hook, a bounded drain is the pragmatic synchronization point.
type recordingNotifier struct {
	srv    *httptest.Server
	posted chan string
}

func newRecordingNotifier(t *testing.T) *recordingNotifier {
	t.Helper()
	rn := &recordingNotifier{
		posted: make(chan string, 16),
		srv: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})),
	}
	t.Cleanup(rn.srv.Close)
	return rn
}

func (r *recordingNotifier) Send(content string) error {
	r.posted <- content
	return nil
}

// withRecordingNotifier swaps in a recording notifier for the duration of fn,
// restoring the original afterwards. sendAlert reads discordNotifier inside its
// fire-and-forget goroutine, so this captures the messages it dispatches by
// draining the buffered channel for a bounded window after fn() returns.
func withRecordingNotifier(t *testing.T, fn func()) []string {
	t.Helper()
	rn := newRecordingNotifier(t)
	orig := discordNotifier
	discordNotifier = rn
	defer func() { discordNotifier = orig }()

	fn()

	var msgs []string
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case m := <-rn.posted:
			msgs = append(msgs, m)
		case <-deadline:
			return msgs
		}
	}
}

// installTestState points the package at a single fake agent and a fresh
// debounce tracker, and clears the shared fleet cache. Tests that need more
// agents override serverAddresses directly.
func installTestState(t *testing.T, fa *fakeAgent) {
	t.Helper()
	alertTracker = alert.NewTracker(1)
	serverAddresses = []string{fa.address()}
	fleetCacheMu.Lock()
	fleetCache = nil
	fleetCacheMu.Unlock()
}

func TestPollSuccess(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)

	res := poll(fa.address())
	if res.Err != "" {
		t.Fatalf("poll: unexpected error %q", res.Err)
	}
	if res.Data == nil || res.Data.Hostname != "node1" {
		t.Fatalf("poll: unexpected data %+v", res)
	}
	if res.Data.CPUUsage != 10.5 {
		t.Errorf("cpu = %v, want 10.5", res.Data.CPUUsage)
	}
	if len(res.Data.Services) != 1 || !res.Data.Services[0].Up {
		t.Errorf("services = %+v, want plex up", res.Data.Services)
	}
}

func TestPollFailureIsReportedNotPanic(t *testing.T) {
	// A dead address must come back as an error result, never a panic.
	res := poll("127.0.0.1:1")
	if res.Err == "" {
		t.Fatal("expected an error for an unreachable agent")
	}
	if res.Data != nil {
		t.Errorf("unreachable agent should have nil data, got %+v", res.Data)
	}
}

func TestPollBadJSONIsReportedNotPanic(t *testing.T) {
	fa := newFakeAgent(t, "bad")
	fa.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{not json"))
	})

	installTestState(t, fa)
	res := poll(fa.address())
	if res.Err == "" {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestPollFailureRecovers(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)

	fa.failNext = true
	first := pollAll(serverAddresses)
	if first[0].Err == "" {
		t.Fatalf("first poll should fail, got %+v", first[0])
	}
	second := pollAll(serverAddresses)
	if second[0].Err != "" {
		t.Fatalf("second poll should recover, got %+v", second[0])
	}
}

func TestPollAllConcurrent(t *testing.T) {
	up := newFakeAgent(t, "up")
	down := newFakeAgent(t, "down")
	installTestState(t, up)

	serverAddresses = []string{up.address(), down.address()}
	fleetCacheMu.Lock()
	fleetCache = nil
	fleetCacheMu.Unlock()

	results := pollAll(serverAddresses)
	if len(results) != 2 {
		t.Fatalf("pollAll returned %d results, want 2", len(results))
	}
	if results[0].Data == nil || results[1].Data == nil {
		t.Fatal("both agents should poll successfully")
	}
	if results[0].Data.Hostname != "up" || results[1].Data.Hostname != "down" {
		t.Errorf("results not index-aligned: %+v", results)
	}
}

func TestCheckAlertsOfflineThenSteady(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)

	// Threshold 1 + a recording notifier capture the alerts sendAlert dispatches.
	alerts := withRecordingNotifier(t, func() {
		alertTracker = alert.NewTracker(1)
		addr := fa.address()

		// First sighting: baseline only, no alert.
		checkAlerts([]AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "node1"}}})
		// Drop offline: alert.
		checkAlerts([]AgentResponse{{Address: addr, Err: "connection refused"}})
		// Steady offline: no repeat.
		checkAlerts([]AgentResponse{{Address: addr, Err: "connection refused"}})
	})

	if len(alerts) != 1 {
		t.Fatalf("expected exactly one offline alert, got %v", alerts)
	}
	if !strings.Contains(alerts[0], "offline") {
		t.Errorf("alert %q should describe an offline transition", alerts[0])
	}
}

func TestCheckAlertsOnlineTransition(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)

	alerts := withRecordingNotifier(t, func() {
		alertTracker = alert.NewTracker(1)
		addr := fa.address()

		// Baseline offline, then recover online: alert fires on recovery.
		checkAlerts([]AgentResponse{{Address: addr, Err: "refused"}})
		checkAlerts([]AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "node1"}}})
	})

	if len(alerts) != 1 {
		t.Fatalf("expected one online alert, got %v", alerts)
	}
	// The alert should name the agent by its address.
	if !strings.Contains(alerts[0], fa.address()) {
		t.Errorf("alert %q should mention the agent address %q", alerts[0], fa.address())
	}
}

func TestFleetHandlerServesCache(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)

	good := poll(fa.address())
	fleetCacheMu.Lock()
	fleetCache = []AgentResponse{good}
	fleetCacheMu.Unlock()

	rr := httptest.NewRecorder()
	fleetHandler(rr, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("fleet handler code = %d, want 200", rr.Code)
	}
	var got []AgentResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode fleet: %v", err)
	}
	if len(got) != 1 || got[0].Data == nil {
		t.Fatalf("fleet handler returned %+v", got)
	}
}

func TestFleetHandlerUnauthenticated(t *testing.T) {
	// RequireAPI gates fleetHandler; without a valid session it must 401.
	store := auth.NewStore("$2a$10$dummy")
	protected := store.RequireAPI(fleetHandler)

	rr := httptest.NewRecorder()
	protected(rr, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth fleet = %d, want 401", rr.Code)
	}
}

func TestGetServerAddressesParsing(t *testing.T) {
	cases := map[string][]string{
		"localhost:8080":    {"localhost:8080"},
		" a:80 , b:9090 ,":  {"a:80", "b:9090"},
		"  , x:1 , , y:2 ,": {"x:1", "y:2"},
		// Empty env falls back to the documented localhost defaults.
		"": {"localhost:8080", "localhost:8081"},
	}
	for in, want := range cases {
		got := getServerAddresses(in)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("getServerAddresses(%q) = %v, want %v", in, got, want)
		}
	}
}
