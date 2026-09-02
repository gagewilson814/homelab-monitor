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
	// restarts records every service name a /restart request asked for, and
	// restartToken captures the agent token header the backend sent.
	restarts     chan string
	restartToken string
	// restartStatus/restartBody let a test script the agent's /restart reply.
	restartStatus int
	restartBody   string
}

func newFakeAgent(t *testing.T, hostname string) *fakeAgent {
	t.Helper()
	fa := &fakeAgent{restarts: make(chan string, 16), restartStatus: http.StatusOK}
	fa.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/restart" {
			fa.restartToken = r.Header.Get(agentTokenHeader)
			var body struct {
				Service string `json:"service"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			fa.restarts <- body.Service
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fa.restartStatus)
			w.Write([]byte(fa.restartBody))
			return
		}
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
			Services:    []stats.ServiceStatus{{Name: "plex", Port: 32400, Up: true, Action: "systemctl restart plex"}},
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
	store := newTestAuthStore(t)
	protected := store.RequireAPI(agentsHandler)

	rr := httptest.NewRecorder()
	protected(rr, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth agents = %d, want 401", rr.Code)
	}
}

func TestFleetHandlerUnauthenticated(t *testing.T) {
	// RequireAPI gates fleetHandler; without a valid session it must 401.
	store := newTestAuthStore(t)
	protected := store.RequireAPI(fleetHandler)

	rr := httptest.NewRecorder()
	protected(rr, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth fleet = %d, want 401", rr.Code)
	}
}

// newTestAuthStore opens a fresh SQLite auth DB in a temp dir seeded with
// an admin/secret user - the replacement for the old single-user hash in a
// test double, exercising the real login path.
func newTestAuthStore(t *testing.T) *auth.Store {
	t.Helper()
	store, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.Seed("admin", "secret"); err != nil {
		t.Fatalf("seed auth store: %v", err)
	}
	return store
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

// serveRestart routes through a real ServeMux so the {id} path value the
// handler reads via r.PathValue is populated exactly as in production.
func serveRestart(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agents/{id}/restart", restartAgentHandler)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// seedRestartState installs a fake agent in the store and seeds fleetCache
// with a fresh poll (which now carries the plex Action), so
// lookupRestartAction finds a configured command.
func seedRestartState(t *testing.T, fa *fakeAgent) {
	t.Helper()
	installTestState(t, fa)
	good := poll(fa.address())
	fleetCacheMu.Lock()
	fleetCache = []AgentResponse{good}
	fleetCacheMu.Unlock()
}

func TestRestartAgentHandlerRelaysToAgent(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	seedRestartState(t, fa)
	fa.restartBody = `{"service":"plex","status":"ok","output":"restarted"}`

	rr := serveRestart(t, httptest.NewRequest(http.MethodPost,
		"/api/agents/"+url.PathEscape(fa.address())+"/restart",
		strings.NewReader(`{"service":"plex"}`)))

	if rr.Code != http.StatusOK {
		t.Fatalf("restart code = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	// The agent must have received exactly the requested service name...
	select {
	case got := <-fa.restarts:
		if got != "plex" {
			t.Errorf("agent saw service %q, want plex", got)
		}
	default:
		t.Fatal("agent never received a /restart request")
	}
	// ...and its JSON body must be relayed verbatim to the caller.
	var relayed struct {
		Service string `json:"service"`
		Status  string `json:"status"`
		Output  string `json:"output"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&relayed); err != nil {
		t.Fatalf("decode relayed body: %v", err)
	}
	if relayed.Service != "plex" || relayed.Status != "ok" || relayed.Output != "restarted" {
		t.Errorf("relayed response = %+v", relayed)
	}
}

func TestRestartAgentHandlerSendsAgentTokenWhenConfigured(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	seedRestartState(t, fa)
	fa.restartBody = `{}`

	orig := agentToken
	agentToken = "s3cret"
	defer func() { agentToken = orig }()

	rr := serveRestart(t, httptest.NewRequest(http.MethodPost,
		"/api/agents/"+url.PathEscape(fa.address())+"/restart",
		strings.NewReader(`{"service":"plex"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("restart code = %d, want 200, body=%s", rr.Code, rr.Body.String())
	}
	if fa.restartToken != "s3cret" {
		t.Errorf("agent saw token %q, want s3cret", fa.restartToken)
	}
}

func TestRestartAgentHandlerUnknownAgent(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	seedRestartState(t, fa)

	rr := serveRestart(t, httptest.NewRequest(http.MethodPost,
		"/api/agents/missing:1/restart", strings.NewReader(`{"service":"plex"}`)))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown agent code = %d, want 404", rr.Code)
	}
}

func TestRestartAgentHandlerServiceWithoutAction(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	seedRestartState(t, fa)

	// The agent advertises plex only; anything else has no configured
	// command and must be refused before ever reaching the agent.
	rr := serveRestart(t, httptest.NewRequest(http.MethodPost,
		"/api/agents/"+url.PathEscape(fa.address())+"/restart",
		strings.NewReader(`{"service":"unconfigured"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured service code = %d, want 400", rr.Code)
	}
	select {
	case got := <-fa.restarts:
		t.Errorf("agent unexpectedly received restart for %q", got)
	default:
	}
}

func TestRestartAgentHandlerRequiresService(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	seedRestartState(t, fa)

	rr := serveRestart(t, httptest.NewRequest(http.MethodPost,
		"/api/agents/"+url.PathEscape(fa.address())+"/restart",
		strings.NewReader(`{"service":"  "}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty service code = %d, want 400", rr.Code)
	}
}

func TestRestartAgentHandlerRelaysAgentFailure(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	seedRestartState(t, fa)
	fa.restartStatus = http.StatusInternalServerError
	fa.restartBody = `{"service":"plex","status":"error","output":"job failed"}`

	rr := serveRestart(t, httptest.NewRequest(http.MethodPost,
		"/api/agents/"+url.PathEscape(fa.address())+"/restart",
		strings.NewReader(`{"service":"plex"}`)))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("relayed failure code = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"status":"error"`) {
		t.Errorf("relayed body = %s, want the agent's error JSON", rr.Body.String())
	}
}

func TestRestartAgentHandlerUnreachableAgent(t *testing.T) {
	// A stored agent that isn't answering: the backend reports a gateway
	// failure rather than hanging or panicking.
	agentStore = newTestAgentStore(t, "127.0.0.1:1")
	fleetCacheMu.Lock()
	fleetCache = []AgentResponse{{
		Address: "127.0.0.1:1",
		Data:    &stats.Response{Hostname: "dead", Services: []stats.ServiceStatus{{Name: "plex", Action: "true"}}},
	}}
	fleetCacheMu.Unlock()

	rr := serveRestart(t, httptest.NewRequest(http.MethodPost,
		"/api/agents/127.0.0.1:1/restart", strings.NewReader(`{"service":"plex"}`)))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("unreachable agent code = %d, want 502, body=%s", rr.Code, rr.Body.String())
	}
}

func TestRestartAgentHandlerMethodNotAllowed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agents/{id}/restart", restartAgentHandler)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/agents/a:1/restart", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code = %d, want 405", rr.Code)
	}
}

func TestRestartAgentHandlerUnauthenticated(t *testing.T) {
	store := newTestAuthStore(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agents/{id}/restart", store.RequireAPI(restartAgentHandler))

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/agents/a:1/restart", strings.NewReader(`{"service":"plex"}`)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth restart = %d, want 401", rr.Code)
	}
}

func TestRestartAgentURL(t *testing.T) {
	if got := restartAgentURL("192.168.1.10:8080"); got != "http://192.168.1.10:8080/restart" {
		t.Errorf("restartAgentURL = %q", got)
	}
}

func TestLookupRestartAction(t *testing.T) {
	fa := newFakeAgent(t, "node1")
	seedRestartState(t, fa)

	if cmd, ok := lookupRestartAction(fa.address(), "plex"); !ok || cmd != "systemctl restart plex" {
		t.Errorf("lookupRestartAction = %q, %v; want the advertised command", cmd, ok)
	}
	if _, ok := lookupRestartAction(fa.address(), "nope"); ok {
		t.Error("lookupRestartAction found an action for an unadvertised service")
	}
	if _, ok := lookupRestartAction("other:1", "plex"); ok {
		t.Error("lookupRestartAction matched the wrong agent address")
	}
}

func TestRestartAgentHandlerOfflineAgent(t *testing.T) {
	// An agent the store knows about but that has no poll data (never
	// reported, or failing auth): restart must fail with a connectivity
	// error, not a config error, and never reach the agent.
	agentStore = newTestAgentStore(t, "offline:1")
	fleetCacheMu.Lock()
	fleetCache = nil
	fleetCacheMu.Unlock()

	rr := serveRestart(t, httptest.NewRequest(http.MethodPost,
		"/api/agents/offline:1/restart", strings.NewReader(`{"service":"plex"}`)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline agent code = %d, want 503, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "agent offline") {
		t.Errorf("body = %s, want an agent-offline message", rr.Body.String())
	}
}

func TestSeedUserCreatesLoginableAccount(t *testing.T) {
	fresh, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	defer fresh.Close()

	// seedUser validates input before touching the store...
	if _, err := seedUser(fresh, "", "pw"); err == nil {
		t.Error("seedUser accepted an empty username")
	}
	if _, err := seedUser(fresh, "ops", ""); err == nil {
		t.Error("seedUser accepted an empty password")
	}

	// ...and creates the account through Store.Seed, so a populated
	// database is refused rather than silently getting a second admin.
	u, err := seedUser(fresh, "ops", "hunter2")
	if err != nil {
		t.Fatalf("seedUser on fresh db: %v", err)
	}
	if u.Username != "ops" {
		t.Errorf("created user = %+v, want username ops", u)
	}
	if _, err := seedUser(fresh, "intruder", "pw2"); err != auth.ErrAlreadySeeded {
		t.Fatalf("seedUser on seeded db err = %v, want auth.ErrAlreadySeeded", err)
	}

	found, ok, err := fresh.FindUserByUsername("ops")
	if err != nil || !ok {
		t.Fatalf("FindUserByUsername: ok=%v err=%v", ok, err)
	}
	if string(found.Password) == "hunter2" {
		t.Error("plaintext password reached the database")
	}
}

func TestLoginLogoutFlowViaHandlers(t *testing.T) {
	store := newTestAuthStore(t)

	// Wrong password: 401, no cookie.
	rr := httptest.NewRecorder()
	store.LoginHandler(rr, httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"username":"admin","password":"wrong"}`)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad login code = %d, want 401", rr.Code)
	}

	// Correct login: 204 + cookie, then /api/me names the user.
	rr = httptest.NewRecorder()
	store.LoginHandler(rr, httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"username":"admin","password":"secret"}`)))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("login code = %d, want 204", rr.Code)
	}
	cookie := rr.Result().Cookies()[0]

	me := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	me.AddCookie(cookie)
	rr = httptest.NewRecorder()
	store.RequireAPI(meHandler)(rr, me)
	if rr.Code != http.StatusOK {
		t.Fatalf("me code = %d, want 200", rr.Code)
	}
	var got struct {
		ID       int    `json:"id"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if got.Username != "admin" {
		t.Errorf("me username = %q, want admin", got.Username)
	}

	// Logout revokes the session; the cookie stops working.
	logout := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	logout.AddCookie(cookie)
	rr = httptest.NewRecorder()
	store.LogoutHandler(rr, logout)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("logout code = %d, want 204", rr.Code)
	}

	me = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	me.AddCookie(cookie)
	rr = httptest.NewRecorder()
	store.RequireAPI(meHandler)(rr, me)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout code = %d, want 401", rr.Code)
	}
}

func TestMeHandlerUnauthenticated(t *testing.T) {
	store := newTestAuthStore(t)
	rr := httptest.NewRecorder()
	store.RequireAPI(meHandler)(rr, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth me = %d, want 401", rr.Code)
	}
}

func TestGetDBFile(t *testing.T) {
	if got := getDBFile(""); got != "data/homelab.db" {
		t.Errorf("getDBFile(\"\") = %q, want the default", got)
	}
	if got := getDBFile("/tmp/test.db"); got != "/tmp/test.db" {
		t.Errorf("getDBFile override = %q, want the override", got)
	}
}

func TestEmptyAuthDBReportsZeroUsers(t *testing.T) {
	// The "backend refuses to start" guard keys off UserCount()==0 on a
	// freshly created database - exercise that path here (runServer itself
	// binds :9090, so the check stays inline there).
	store, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open empty auth db: %v", err)
	}
	defer store.Close()

	if n, err := store.UserCount(); err != nil || n != 0 {
		t.Fatalf("UserCount on fresh db = %d, %v; want 0", n, err)
	}
}

func TestLoginRateLimitedViaBackendWiring(t *testing.T) {
	// Mirror production wiring: LimitedLoginHandler around the store, 10
	// attempts per 15s.
	store := newTestAuthStore(t)
	limiter := auth.NewLoginRateLimiter(10, 15*time.Second)
	handler := auth.LimitedLoginHandler(store, limiter)

	attempt := func(password string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		handler(rr, httptest.NewRequest(http.MethodPost, "/api/login",
			strings.NewReader(`{"username":"admin","password":"`+password+`"}`)))
		return rr
	}

	for i := 0; i < 10; i++ {
		if rr := attempt("wrong"); rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: code=%d, want 401", i+1, rr.Code)
		}
	}
	rr := attempt("secret")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt 11: code=%d, want 429", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("429 content-type = %q, want application/json", ct)
	}
	if body := rr.Body.String(); !strings.Contains(body, "too many attempts") {
		t.Errorf("429 body = %q", body)
	}
}

func TestLoginRateLimitDoesNotAffectOtherRoutes(t *testing.T) {
	// Fleet is wrapped in RequireAPI only - no limiter in the chain.
	store := newTestAuthStore(t)
	cookie := loginForCookie(t, store)

	mux := http.NewServeMux()
	limiter := auth.NewLoginRateLimiter(1, 15*time.Second)
	mux.HandleFunc("/api/login", auth.LimitedLoginHandler(store, limiter))
	mux.HandleFunc("/api/fleet", store.RequireAPI(fleetHandler))

	// Burn the limiter's whole window on login...
	for i := 0; i < 1; i++ {
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/login",
			strings.NewReader(`{"username":"admin","password":"wrong"}`)))
	}
	// ...and confirm /api/fleet is untouched by it.
	req := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("fleet through limiter-exhausted mux: code=%d, want 200", rr.Code)
	}
}

func TestRestartRejectsCrossSiteOrigin(t *testing.T) {
	store := newTestAuthStore(t)
	cookie := loginForCookie(t, store)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agents/{id}/restart", store.RequireAPI(requireSameOrigin(restartAgentHandler)))

	req := httptest.NewRequest(http.MethodPost, "/api/agents/localhost:1/restart",
		strings.NewReader(`{"service":"x"}`))
	req.Host = "dashboard.example"
	req.AddCookie(cookie)
	req.Header.Set("Origin", "https://evil.example")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin restart: code=%d, want 403", rr.Code)
	}
}

func TestRestartAllowsSameOrigin(t *testing.T) {
	store := newTestAuthStore(t)
	cookie := loginForCookie(t, store)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agents/{id}/restart", store.RequireAPI(requireSameOrigin(restartAgentHandler)))

	// Origin matching Host passes the check (then fails later on unknown
	// agent - the point is it got PAST the 403).
	req := httptest.NewRequest(http.MethodPost, "/api/agents/localhost:1/restart",
		strings.NewReader(`{"service":"x"}`))
	req.Host = "dashboard.example"
	req.AddCookie(cookie)
	req.Header.Set("Origin", "https://dashboard.example")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code == http.StatusForbidden {
		t.Fatal("same-origin restart was rejected by the Origin check")
	}
}

func TestLogoutRejectsCrossSiteOrigin(t *testing.T) {
	store := newTestAuthStore(t)

	req := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	req.Host = "dashboard.example"
	req.Header.Set("Origin", "https://evil.example")

	rr := httptest.NewRecorder()
	requireSameOrigin(store.LogoutHandler)(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin logout: code=%d, want 403", rr.Code)
	}
}

func TestLogoutAllowsNoOriginHeader(t *testing.T) {
	// Same-origin tools (curl, non-browser clients) send no Origin; the
	// check must only fire on a present-and-mismatched one.
	store := newTestAuthStore(t)
	rr := httptest.NewRecorder()
	requireSameOrigin(store.LogoutHandler)(rr, httptest.NewRequest(http.MethodPost, "/api/logout", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("no-Origin logout: code=%d, want 204", rr.Code)
	}
}

// loginForCookie logs into store as admin and returns the session cookie.
func loginForCookie(t *testing.T, store *auth.Store) *http.Cookie {
	t.Helper()
	rr := httptest.NewRecorder()
	store.LoginHandler(rr, httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"username":"admin","password":"secret"}`)))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("login for cookie: code=%d", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "homelab_session" {
			return c
		}
	}
	t.Fatal("no session cookie from login")
	return nil
}

func TestLoginOversizedBodyIs413ViaBackendWiring(t *testing.T) {
	store := newTestAuthStore(t)
	handler := auth.LimitedLoginHandler(store, auth.NewLoginRateLimiter(10, 15*time.Second))

	huge := `{"username":"admin","password":"` + strings.Repeat("a", 200*1024) + `"}`
	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(huge)))

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body via wrapper: code=%d, want 413", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "too large") {
		t.Errorf("413 body = %q", body)
	}
}
