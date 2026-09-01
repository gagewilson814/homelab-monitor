package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"homeserver-monitor/internal/agentstore"
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
	agentStore = newTestAgentStore(t, fa.address())
	fleetCacheMu.Lock()
	fleetCache = nil
	fleetCacheMu.Unlock()
	alertsCacheMu.Lock()
	alertsCache = nil
	alertsCacheMu.Unlock()
	historyMu.Lock()
	historyCache = make(map[string]*MetricHistory)
	historyMu.Unlock()
}

// newTestAgentStore returns a fresh, temp-file-backed agentstore.Store
// seeded with addresses - the same store fleetHandler/agentsHandler/
// checkAlerts consult via the package-level agentStore var.
func newTestAgentStore(t *testing.T, addresses ...string) *agentstore.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.json")
	store, err := agentstore.Load(path, addresses)
	if err != nil {
		t.Fatalf("load test agent store: %v", err)
	}
	return store
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

func TestPollSendsAgentTokenWhenConfigured(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(agentTokenHeader)
		json.NewEncoder(w).Encode(stats.Response{Hostname: "node1"})
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	orig := agentToken
	agentToken = "s3cret"
	defer func() { agentToken = orig }()

	if res := poll(addr); res.Err != "" {
		t.Fatalf("poll: unexpected error %q", res.Err)
	}
	if gotHeader != "s3cret" {
		t.Errorf("agent saw token header %q, want %q", gotHeader, "s3cret")
	}
}

func TestPollSendsNoTokenHeaderByDefault(t *testing.T) {
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get(agentTokenHeader) != ""
		json.NewEncoder(w).Encode(stats.Response{Hostname: "node1"})
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	orig := agentToken
	agentToken = ""
	defer func() { agentToken = orig }()

	if res := poll(addr); res.Err != "" {
		t.Fatalf("poll: unexpected error %q", res.Err)
	}
	if sawHeader {
		t.Error("poll sent a token header even though agentToken is unset")
	}
}

func TestSecurityHeadersSetsCSP(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rr := httptest.NewRecorder()
	securityHeaders(inner).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	got := rr.Header().Get("Content-Security-Policy")
	if got == "" {
		t.Fatal("securityHeaders did not set a Content-Security-Policy header")
	}
	if !strings.Contains(got, "script-src 'self'") {
		t.Errorf("CSP = %q, want script-src 'self' with no unsafe-inline", got)
	}
	if strings.Contains(got, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP allows unsafe-inline scripts: %q", got)
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

func TestCheckAlertsThresholdBreachAndRecovery(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)

	origCPU := cpuThreshold
	cpuThreshold = 80
	defer func() { cpuThreshold = origCPU }()

	alerts := withRecordingNotifier(t, func() {
		alertTracker = alert.NewTracker(1)
		addr := fa.address()

		// Baseline healthy, then a breach: alert fires once on the breach.
		checkAlerts([]AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "node1", CPUUsage: 10}}})
		checkAlerts([]AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "node1", CPUUsage: 95}}})
		// Steady breach: no repeat.
		checkAlerts([]AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "node1", CPUUsage: 96}}})
		// Recovery: alert fires once more.
		checkAlerts([]AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "node1", CPUUsage: 5}}})
	})

	if len(alerts) != 2 {
		t.Fatalf("expected a breach alert and a recovery alert, got %v", alerts)
	}
	// sendAlert fires each alert from its own goroutine, so the two messages
	// can land in either order - assert on content, not position.
	joined := strings.Join(alerts, " | ")
	if !strings.Contains(joined, "CPU") || !strings.Contains(joined, "high") {
		t.Errorf("expected a high-CPU breach alert, got %v", alerts)
	}
	if !strings.Contains(joined, "normal") {
		t.Errorf("expected a recovery-to-normal alert, got %v", alerts)
	}
}

func TestAnnotateLastSeenPersistsThroughOutage(t *testing.T) {
	addr := "last-seen-test:1"
	lastSeenMu.Lock()
	delete(lastSeenCache, addr)
	lastSeenMu.Unlock()

	online := []AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "n"}}}
	annotateLastSeen(online)
	if online[0].LastSeen == nil {
		t.Fatal("expected LastSeen to be set for an online result")
	}
	seenAt := *online[0].LastSeen

	offline := []AgentResponse{{Address: addr, Err: "connection refused"}}
	annotateLastSeen(offline)
	if offline[0].LastSeen == nil || !offline[0].LastSeen.Equal(seenAt) {
		t.Fatalf("expected the offline result to retain the last-online timestamp, got %v want %v", offline[0].LastSeen, seenAt)
	}
}

func TestAnnotateHistoryAppendsAndCaps(t *testing.T) {
	addr := "history-test:1"
	historyMu.Lock()
	delete(historyCache, addr)
	historyMu.Unlock()

	for i := 0; i < maxHistoryPoints+5; i++ {
		results := []AgentResponse{{Address: addr, Data: &stats.Response{CPUUsage: float64(i)}}}
		annotateHistory(results)
		if len(results[0].History.CPU) > maxHistoryPoints {
			t.Fatalf("history grew past cap: len=%d", len(results[0].History.CPU))
		}
	}

	results := []AgentResponse{{Address: addr, Data: &stats.Response{CPUUsage: 999}}}
	annotateHistory(results)
	got := results[0].History.CPU
	if len(got) != maxHistoryPoints {
		t.Fatalf("history len = %d, want %d", len(got), maxHistoryPoints)
	}
	if got[len(got)-1] != 999 {
		t.Fatalf("newest sample = %v, want 999 as the last element", got[len(got)-1])
	}
}

func TestAnnotateHistoryFreezesOnOffline(t *testing.T) {
	addr := "history-offline-test:1"
	historyMu.Lock()
	delete(historyCache, addr)
	historyMu.Unlock()

	online := []AgentResponse{{Address: addr, Data: &stats.Response{CPUUsage: 42}}}
	annotateHistory(online)

	offline := []AgentResponse{{Address: addr, Err: "connection refused"}}
	annotateHistory(offline)

	if len(offline[0].History.CPU) != 1 || offline[0].History.CPU[0] != 42 {
		t.Fatalf("offline poll should keep prior history unchanged, got %+v", offline[0].History)
	}
}

func TestCheckAlertsReturnsActiveOfflineAlert(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)

	_ = withRecordingNotifier(t, func() {
		alertTracker = alert.NewTracker(1)
		addr := fa.address()

		checkAlerts([]AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "node1"}}})
		alerts := checkAlerts([]AgentResponse{{Address: addr, Err: "connection refused"}})

		if len(alerts) != 1 {
			t.Fatalf("expected exactly one active alert, got %v", alerts)
		}
		if alerts[0].Type != AlertOffline {
			t.Errorf("alert type = %q, want %q", alerts[0].Type, AlertOffline)
		}
		if alerts[0].Since.IsZero() {
			t.Error("alert Since should be set")
		}

		// The alert should keep appearing while the outage is ongoing, not
		// just on the poll where it started.
		steady := checkAlerts([]AgentResponse{{Address: addr, Err: "connection refused"}})
		if len(steady) != 1 {
			t.Fatalf("expected the offline alert to persist across polls, got %v", steady)
		}
	})
}

func TestCheckAlertsThresholdAlertClearsOnRecovery(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)

	origCPU := cpuThreshold
	cpuThreshold = 80
	defer func() { cpuThreshold = origCPU }()

	_ = withRecordingNotifier(t, func() {
		alertTracker = alert.NewTracker(1)
		addr := fa.address()

		checkAlerts([]AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "node1", CPUUsage: 10}}})
		breach := checkAlerts([]AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "node1", CPUUsage: 95}}})
		if len(breach) != 1 || breach[0].Type != AlertThreshold {
			t.Fatalf("expected one threshold alert, got %v", breach)
		}

		recovered := checkAlerts([]AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "node1", CPUUsage: 5}}})
		if len(recovered) != 0 {
			t.Fatalf("expected no active alerts after recovery, got %v", recovered)
		}
	})
}

func TestAlertsHandlerServesCache(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)

	want := []Alert{{Type: AlertOffline, Target: "node1", Message: "node1 is offline", Since: time.Now()}}
	alertsCacheMu.Lock()
	alertsCache = want
	alertsCacheMu.Unlock()

	rr := httptest.NewRecorder()
	alertsHandler(rr, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("alerts handler code = %d, want 200", rr.Code)
	}
	var got []Alert
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode alerts: %v", err)
	}
	if len(got) != 1 || got[0].Type != AlertOffline || got[0].Target != "node1" {
		t.Fatalf("alerts handler returned %+v", got)
	}
}

func TestAlertsHandlerServesEmptyArrayNotNull(t *testing.T) {
	alertsCacheMu.Lock()
	alertsCache = nil
	alertsCacheMu.Unlock()

	rr := httptest.NewRecorder()
	alertsHandler(rr, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
	if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
		t.Fatalf("empty alerts body = %q, want []", got)
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

func TestFleetHandlerReflectsTagImmediately(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)

	good := poll(fa.address())
	fleetCacheMu.Lock()
	fleetCache = []AgentResponse{good}
	fleetCacheMu.Unlock()

	if err := agentStore.SetTag(fa.address(), "Plex server"); err != nil {
		t.Fatalf("SetTag: %v", err)
	}

	rr := httptest.NewRecorder()
	fleetHandler(rr, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
	var got []AgentResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode fleet: %v", err)
	}
	if len(got) != 1 || got[0].Tag != "Plex server" {
		t.Fatalf("fleet handler tag = %+v, want Plex server reflected without a re-poll", got)
	}
}

func TestFleetHandlerFiltersRemovedAgent(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)

	good := poll(fa.address())
	fleetCacheMu.Lock()
	fleetCache = []AgentResponse{good}
	fleetCacheMu.Unlock()

	if err := agentStore.Remove(fa.address()); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	rr := httptest.NewRecorder()
	fleetHandler(rr, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
	var got []AgentResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode fleet: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fleet handler should filter out a removed agent without a re-poll, got %+v", got)
	}
}

func TestAgentsHandlerGetListsConfigured(t *testing.T) {
	agentStore = newTestAgentStore(t, "a:1", "b:2")

	rr := httptest.NewRecorder()
	agentsHandler(rr, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET code = %d, want 200", rr.Code)
	}
	var got []agentstore.Agent
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("agents = %+v, want 2", got)
	}
}

func TestAgentsHandlerPostAddsAgent(t *testing.T) {
	agentStore = newTestAgentStore(t)

	body := strings.NewReader(`{"address":"192.168.1.10:8080","tag":"NAS"}`)
	rr := httptest.NewRecorder()
	agentsHandler(rr, httptest.NewRequest(http.MethodPost, "/api/agents", body))
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST code = %d, want 201, body=%s", rr.Code, rr.Body.String())
	}

	var got agentstore.Agent
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Address != "192.168.1.10:8080" || got.Tag != "NAS" {
		t.Fatalf("created agent = %+v", got)
	}
	if !agentStore.Has("192.168.1.10:8080") {
		t.Fatal("agent should be persisted in the store after POST")
	}
}

func TestAgentsHandlerPostRejectsInvalidAddress(t *testing.T) {
	agentStore = newTestAgentStore(t)

	body := strings.NewReader(`{"address":"not-an-address"}`)
	rr := httptest.NewRecorder()
	agentsHandler(rr, httptest.NewRequest(http.MethodPost, "/api/agents", body))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST invalid address code = %d, want 400", rr.Code)
	}
}

func TestAgentsHandlerPostRejectsDuplicate(t *testing.T) {
	agentStore = newTestAgentStore(t, "a:1")

	body := strings.NewReader(`{"address":"a:1"}`)
	rr := httptest.NewRecorder()
	agentsHandler(rr, httptest.NewRequest(http.MethodPost, "/api/agents", body))
	if rr.Code != http.StatusConflict {
		t.Fatalf("POST duplicate code = %d, want 409", rr.Code)
	}
}

func TestAgentsHandlerPutUpdatesTag(t *testing.T) {
	agentStore = newTestAgentStore(t, "a:1")

	body := strings.NewReader(`{"address":"a:1","tag":"Backup box"}`)
	rr := httptest.NewRecorder()
	agentsHandler(rr, httptest.NewRequest(http.MethodPut, "/api/agents", body))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("PUT code = %d, want 204, body=%s", rr.Code, rr.Body.String())
	}
	if got := agentStore.Tag("a:1"); got != "Backup box" {
		t.Errorf("tag = %q, want %q", got, "Backup box")
	}
}

func TestAgentsHandlerPutUnknownAddress(t *testing.T) {
	agentStore = newTestAgentStore(t)

	body := strings.NewReader(`{"address":"missing:1","tag":"x"}`)
	rr := httptest.NewRecorder()
	agentsHandler(rr, httptest.NewRequest(http.MethodPut, "/api/agents", body))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("PUT unknown address code = %d, want 404", rr.Code)
	}
}

func TestAgentsHandlerDeleteRemovesAgent(t *testing.T) {
	agentStore = newTestAgentStore(t, "a:1")

	rr := httptest.NewRecorder()
	agentsHandler(rr, httptest.NewRequest(http.MethodDelete, "/api/agents?address=a:1", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE code = %d, want 204, body=%s", rr.Code, rr.Body.String())
	}
	if agentStore.Has("a:1") {
		t.Error("agent should be gone from the store after DELETE")
	}
}

func TestAgentsHandlerDeleteClearsPerAgentState(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	installTestState(t, fa)
	addr := fa.address()

	// Build up the state a live agent accumulates: hostname, last-seen,
	// sparkline history and debounce state.
	results := []AgentResponse{{Address: addr, Data: &stats.Response{Hostname: "node1", CPUUsage: 12}}}
	annotateLastSeen(results)
	annotateHistory(results)
	_ = withRecordingNotifier(t, func() { checkAlerts(results) })

	if _, ok := lookupLastSeen(addr); !ok {
		t.Fatal("precondition: expected last-seen to be recorded")
	}

	rr := httptest.NewRecorder()
	agentsHandler(rr, httptest.NewRequest(http.MethodDelete, "/api/agents?address="+addr, nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE code = %d, want 204", rr.Code)
	}

	// Re-adding the same address must not inherit the old agent's data.
	if got := lookupHostname(addr); got != "" {
		t.Errorf("hostname cache still holds %q after removal", got)
	}
	if _, ok := lookupLastSeen(addr); ok {
		t.Error("last-seen cache still holds an entry after removal")
	}
	historyMu.Lock()
	_, hasHistory := historyCache[addr]
	historyMu.Unlock()
	if hasHistory {
		t.Error("history cache still holds an entry after removal")
	}
	if _, _, ok := alertTracker.Confirmed(addr); ok {
		t.Error("alert tracker still holds state after removal")
	}
}

func TestAgentsHandlerDeleteUnknownAddress(t *testing.T) {
	agentStore = newTestAgentStore(t)

	rr := httptest.NewRecorder()
	agentsHandler(rr, httptest.NewRequest(http.MethodDelete, "/api/agents?address=missing:1", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown address code = %d, want 404", rr.Code)
	}
}

func TestAgentsHandlerMethodNotAllowed(t *testing.T) {
	agentStore = newTestAgentStore(t)

	rr := httptest.NewRecorder()
	agentsHandler(rr, httptest.NewRequest(http.MethodPatch, "/api/agents", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH code = %d, want 405", rr.Code)
	}
}

func TestAgentsHandlerUnauthenticated(t *testing.T) {
	store := auth.NewStore("$2a$10$dummy")
	protected := store.RequireAPI(agentsHandler)

	rr := httptest.NewRecorder()
	protected(rr, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth agents = %d, want 401", rr.Code)
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
