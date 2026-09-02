// Command backend is the central service. It lists the agents to monitor
// (from the HOMELAB_AGENTS env var), polls them all at once with a
// goroutine per agent, aggregates the results, and serves the fleet as JSON
// at /api/fleet. It also serves the static web dashboard from ./web.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"homeserver-monitor/internal/agentstore"
	"homeserver-monitor/internal/alert"
	"homeserver-monitor/internal/auth"
	"homeserver-monitor/internal/notify"
	"homeserver-monitor/internal/stats"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// serverAddresses is the *initial* agent list, parsed from HOMELAB_AGENTS.
// It only matters the first time the process runs: agentStore persists
// whatever list is actually in effect (including agents added or removed
// from the dashboard) to disk from then on, so this env var stops being
// consulted once that file exists.
var serverAddresses = getServerAddresses(os.Getenv("HOMELAB_AGENTS"))

// agentStore is the persisted, mutable set of monitored agents (address +
// tag). It's nil until main() loads it, then read by pollLoop every cycle
// and read/written by agentsHandler in response to dashboard edits.
var agentStore *agentstore.Store

// maxBodyBytes caps how much of a JSON request body the agents API will
// read, so a malformed or hostile client can't force a huge allocation.
const maxBodyBytes = 64 << 10

// agentTokenHeader is the header poll() sends its shared secret in - the
// name must match cmd/agent's homeHandler, which checks the same literal
// string independently (the two binaries don't share a config package).
const agentTokenHeader = "X-Homelab-Agent-Token"

// agentToken is the shared secret sent with every poll, if any. Left
// unset (the default), poll() sends no such header - identical to its
// behavior before this existed, for anyone not opting in. Set it to the
// same value as HOMELAB_AGENT_TOKEN on every agent that requires it.
var agentToken = os.Getenv("HOMELAB_AGENT_TOKEN")

// getAgentsFile reads HOMELAB_AGENTS_FILE, the JSON file that persists the
// monitored agent list (address + tag) across restarts, defaulting to
// data/agents.json under the working directory.
func getAgentsFile(env string) string {
	if env != "" {
		return env
	}
	return "data/agents.json"
}

// discordNotifier sends the up/down alerts. Its webhook URL is optional -
// an empty DISCORD_WEBHOOK_URL just makes Send a no-op, so alerting is
// silently disabled rather than requiring its own feature flag. Declared as
// the Notifier interface (not the concrete *Discord) so tests can swap in a
// recording double.
var discordNotifier notify.Notifier = notify.NewDiscord(os.Getenv("DISCORD_WEBHOOK_URL"))

// alertTracker debounces poll results so a single dropped poll doesn't fire
// a notification; see getAlertThreshold for how many consecutive results in
// the same direction it takes to confirm a transition.
var alertTracker = alert.NewTracker(getAlertThreshold())

// hostnameCache remembers the last hostname seen for each agent address, so
// an offline alert (which by definition has no fresh stats.Response to read
// a hostname from) can still name the machine instead of just its address.
var (
	hostnameMu    sync.Mutex
	hostnameCache = make(map[string]string)
)

func rememberHostname(address string, data *stats.Response) {
	if data == nil {
		return
	}
	hostnameMu.Lock()
	hostnameCache[address] = data.Hostname
	hostnameMu.Unlock()
}

func lookupHostname(address string) string {
	hostnameMu.Lock()
	defer hostnameMu.Unlock()
	return hostnameCache[address]
}

// displayName returns the friendliest available label for an agent: its
// user-assigned tag if it has one, else its last-known hostname, else the
// bare address - each suffixed with the address so alerts/messages stay
// unambiguous even when two agents share a tag or hostname.
func displayName(address string) string {
	if tag := agentStore.Tag(address); tag != "" {
		return fmt.Sprintf("%s (%s)", tag, address)
	}
	if hostname := lookupHostname(address); hostname != "" {
		return fmt.Sprintf("%s (%s)", hostname, address)
	}
	return address
}

// lastSeenCache remembers when each agent was last observed online, so an
// offline card can show when it was last healthy instead of just "down".
var (
	lastSeenMu    sync.Mutex
	lastSeenCache = make(map[string]time.Time)
)

func rememberLastSeen(address string, online bool) {
	if !online {
		return
	}
	lastSeenMu.Lock()
	lastSeenCache[address] = time.Now()
	lastSeenMu.Unlock()
}

func lookupLastSeen(address string) (time.Time, bool) {
	lastSeenMu.Lock()
	defer lastSeenMu.Unlock()
	t, ok := lastSeenCache[address]
	return t, ok
}

// annotateLastSeen records this round's online agents into lastSeenCache,
// then stamps every result (online or not) with the most recent time it was
// seen online, so an offline card can still report when it was last healthy.
func annotateLastSeen(results []AgentResponse) {
	for i := range results {
		online := results[i].Err == ""
		rememberLastSeen(results[i].Address, online)
		if t, ok := lookupLastSeen(results[i].Address); ok {
			results[i].LastSeen = &t
		}
	}
}

// maxHistoryPoints caps how many samples of a metric are kept per agent -
// enough for a several-minute sparkline without the cache growing forever.
const maxHistoryPoints = 60

// MetricHistory is a short ring buffer of recent CPU/memory/disk samples for
// one agent, oldest first, used to render a trend sparkline on the
// dashboard. It's kept as a separate cache (not stats.Response) because it's
// aggregation state the backend owns, not something an Agent reports.
type MetricHistory struct {
	CPU  []float64 `json:"cpu"`
	Mem  []float64 `json:"mem"`
	Disk []float64 `json:"disk"`
}

// historyCache holds one MetricHistory per agent address, appended to on
// every successful poll.
var (
	historyMu    sync.Mutex
	historyCache = make(map[string]*MetricHistory)
)

// recordHistory appends this poll's metrics (if data was collected) into the
// agent's history ring buffer and returns a snapshot copy of it. Offline
// polls (data == nil) still return the existing history unchanged, so an
// offline card keeps showing the trend leading up to the outage instead of
// losing it.
func recordHistory(address string, data *stats.Response) *MetricHistory {
	historyMu.Lock()
	defer historyMu.Unlock()

	h, ok := historyCache[address]
	if !ok {
		h = &MetricHistory{}
		historyCache[address] = h
	}
	if data != nil {
		h.CPU = appendCapped(h.CPU, data.CPUUsage)
		h.Mem = appendCapped(h.Mem, data.MemoryUsage)
		h.Disk = appendCapped(h.Disk, data.DiskUsage)
	}

	return &MetricHistory{
		CPU:  append([]float64(nil), h.CPU...),
		Mem:  append([]float64(nil), h.Mem...),
		Disk: append([]float64(nil), h.Disk...),
	}
}

// appendCapped appends v to s, dropping from the front once s exceeds
// maxHistoryPoints so the buffer stays a fixed-size sliding window.
func appendCapped(s []float64, v float64) []float64 {
	s = append(s, v)
	if len(s) > maxHistoryPoints {
		s = s[len(s)-maxHistoryPoints:]
	}
	return s
}

// forgetAgent drops every piece of per-agent state the backend accumulates
// outside agentStore. Without this, removing an agent and later re-adding
// the same address would resurrect its old sparkline history, a stale
// "last seen" from its previous life, and its previous alert state - all
// presented as if they belonged to the freshly-added agent.
func forgetAgent(address string) {
	hostnameMu.Lock()
	delete(hostnameCache, address)
	hostnameMu.Unlock()

	lastSeenMu.Lock()
	delete(lastSeenCache, address)
	lastSeenMu.Unlock()

	historyMu.Lock()
	delete(historyCache, address)
	historyMu.Unlock()

	alertTracker.Forget(address)
}

// annotateHistory stamps every result with its updated metric history. Must
// run after rememberHostname/annotateLastSeen-style bookkeeping is otherwise
// unrelated to poll ordering, but is kept as its own pass for the same
// reason annotateLastSeen is: one clear responsibility per pass over
// results.
func annotateHistory(results []AgentResponse) {
	for i := range results {
		results[i].History = recordHistory(results[i].Address, results[i].Data)
	}
}

// getAlertThreshold reads DISCORD_ALERT_THRESHOLD, defaulting to 2
// consecutive polls (see getPollInterval for how often that is).
func getAlertThreshold() int {
	if env := os.Getenv("DISCORD_ALERT_THRESHOLD"); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 {
			return n
		}
	}
	return 2
}

// cpuThreshold, memThreshold, and diskThreshold are the sustained-usage
// percentages that trigger a threshold alert (see checkMetricThreshold).
var (
	cpuThreshold  = getMetricThreshold("HOMELAB_CPU_THRESHOLD", 90)
	memThreshold  = getMetricThreshold("HOMELAB_MEM_THRESHOLD", 90)
	diskThreshold = getMetricThreshold("HOMELAB_DISK_THRESHOLD", 90)
)

// getMetricThreshold reads a percentage threshold from the given env var,
// falling back to def if it's unset or not a positive number.
func getMetricThreshold(env string, def float64) float64 {
	if v := os.Getenv(env); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return def
}

// getPollInterval reads HOMELAB_POLL_INTERVAL (seconds), defaulting to 5.
// This is how often the backend polls agents on its own, independent of
// whether anyone has the dashboard open.
func getPollInterval() time.Duration {
	if env := os.Getenv("HOMELAB_POLL_INTERVAL"); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 5 * time.Second
}

// getServerAddresses parses the comma-separated HOMELAB_AGENTS env var into a
// list of host:port addresses, trimming whitespace and dropping empties. A
// single parameter (rather than os.Getenv inside) keeps it unit-testable.
func getServerAddresses(env string) []string {
	if env != "" {
		var addresses []string
		for _, addr := range strings.Split(env, ",") {
			addr = strings.TrimSpace(addr)
			if addr != "" {
				addresses = append(addresses, addr)
			}
		}
		return addresses
	}
	return []string{"localhost:8080", "localhost:8081"}
}

// AgentResponse is the per-agent result. Data holds a successful scrape,
// while Err carries the failure reason so the dashboard can still render
// the card (marked offline) even when an agent is unreachable.
type AgentResponse struct {
	Address  string          `json:"address"`
	Data     *stats.Response `json:"data,omitempty"`
	Err      string          `json:"error,omitempty"`
	LastSeen *time.Time      `json:"last_seen,omitempty"`
	History  *MetricHistory  `json:"history,omitempty"`
	// Tag is stamped in at serve time (see fleetHandler), not poll time, so
	// editing a tag from the dashboard is reflected immediately rather than
	// waiting for the next background poll.
	Tag string `json:"tag,omitempty"`
}

// poll fetches a single agent's /stats endpoint and decodes it. Any of the
// four failure modes (bad request construction, network, non-200 status,
// bad JSON) are reported via Err rather than panicking, so one bad agent
// can't sink the whole fleet.
func poll(server string) AgentResponse {

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequest(http.MethodGet, "http://"+server+"/stats", nil)
	if err != nil {
		return AgentResponse{Address: server, Err: err.Error()}
	}
	if agentToken != "" {
		req.Header.Set(agentTokenHeader, agentToken)
	}

	response, err := client.Do(req)
	if err != nil {
		return AgentResponse{Address: server, Err: err.Error()}
	}

	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return AgentResponse{Address: server, Err: fmt.Sprintf("unexpected status: %s", response.Status)}
	}

	var statsResponse stats.Response
	err = json.NewDecoder(response.Body).Decode(&statsResponse)
	if err != nil {
		return AgentResponse{Address: server, Err: err.Error()}
	}

	return AgentResponse{Address: server, Data: &statsResponse}
}

// pollAll runs every agent concurrently: one goroutine per agent that writes
// its result into results[i]. The WaitGroup ensures all goroutines have
// finished before the slice is returned, so the indices stay race-free.
func pollAll(servers []string) []AgentResponse {
	results := make([]AgentResponse, len(servers))

	var wg sync.WaitGroup
	for i, server := range servers {
		wg.Add(1)
		go func(i int, server string) {
			defer wg.Done()
			results[i] = poll(server)
		}(i, server)
	}
	wg.Wait()

	return results
}

// fleetCache holds the most recent poll results. A background goroutine
// (see pollLoop) is the sole writer, polling on its own schedule so alerting
// keeps running whether or not anyone has the dashboard open; fleetHandler
// just serves whatever it last wrote.
var (
	fleetCacheMu sync.RWMutex
	fleetCache   []AgentResponse
)

// alertsCache holds the most recently computed set of currently-active
// alerts (offline agents, down services, sustained threshold breaches).
// Like fleetCache, it's written once per poll cycle by pollLoop and served
// as-is by alertsHandler.
var (
	alertsCacheMu sync.RWMutex
	alertsCache   []Alert
)

// AlertType categorizes an entry in the aggregated /api/alerts feed.
type AlertType string

const (
	AlertOffline   AlertType = "offline"
	AlertService   AlertType = "service"
	AlertThreshold AlertType = "threshold"
)

// Alert is one currently-active problem the debounce tracker has confirmed:
// an offline agent, a down service, or a sustained CPU/memory/disk
// threshold breach. Unlike the Discord notifications checkAlerts also
// fires, this list reflects present-tense state - an entry stays in it for
// as long as the problem is ongoing, not just the poll where it started.
type Alert struct {
	Type    AlertType `json:"type"`
	Target  string    `json:"target"`
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
}

// pollLoop polls every agent and runs alert checks on a fixed interval,
// forever. It's meant to run in its own goroutine for the life of the
// process.
func pollLoop(interval time.Duration) {
	for {
		results := pollAll(agentStore.Addresses())
		annotateLastSeen(results)
		annotateHistory(results)
		alerts := checkAlerts(results)

		fleetCacheMu.Lock()
		fleetCache = results
		fleetCacheMu.Unlock()

		alertsCacheMu.Lock()
		alertsCache = alerts
		alertsCacheMu.Unlock()

		time.Sleep(interval)
	}
}

// restartAgentTimeout is how long the backend will wait on an agent's
// /restart endpoint. The agent itself gives the restart command 60s, so
// this bounds the backend's exposure to a hung agent rather than the
// command runtime.
const restartAgentTimeout = 10 * time.Second

// restartAgentURL builds the agent /restart endpoint URL from a stored
// agent address (host:port, the same "id" the agent store keys on).
func restartAgentURL(address string) string {
	return "http://" + address + "/restart"
}

// lastPoll returns the most recent cached poll result for address, or nil
// if the agent has never reported in (offline, unauthorized, or brand new).
func lastPoll(address string) *stats.Response {
	fleetCacheMu.RLock()
	results := fleetCache
	fleetCacheMu.RUnlock()

	for _, res := range results {
		if res.Address == address {
			return res.Data
		}
	}
	return nil
}

// lookupRestartAction finds the restart command last advertised for service
// on address in the cached fleet poll, so the request body sent to the
// agent echoes agent-published config. The agent itself re-validates the
// service against its own configured actions and never executes a command
// it receives over the wire.
func lookupRestartAction(address, service string) (string, bool) {
	data := lastPoll(address)
	if data == nil {
		return "", false
	}
	for _, svc := range data.Services {
		if svc.Name == service && svc.Action != "" {
			return svc.Action, true
		}
	}
	return "", false
}

// restartAgentHandler relays a restart request for one service on one agent
// (the {id} path value is the agent's address, the same key the agentstore
// and dashboard use). It is auth-gated like every other /api endpoint; the
// agent additionally guards /restart with its own shared token when
// configured. The agent's JSON response - success or failure - is relayed
// to the dashboard as-is so the operator sees the command's real output.
func restartAgentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	address := r.PathValue("id")
	if !agentStore.Has(address) {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Service string `json:"service"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	service := strings.TrimSpace(body.Service)
	if service == "" {
		http.Error(w, "service is required", http.StatusBadRequest)
		return
	}

	command, ok := lookupRestartAction(address, service)
	if !ok {
		// Distinguish "we've never heard from this agent" (connectivity or
		// auth problem) from "the agent didn't advertise a restart action
		// for this service" (configuration), so the error tells the operator
		// which of the two to go fix.
		if lastPoll(address) == nil {
			http.Error(w, "agent offline - no recent poll data", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "no restart action configured for service", http.StatusBadRequest)
		return
	}

	// "command" is an echo of the agent's own published config, sent for
	// observability only: the agent decodes just the service name and
	// re-validates it against HOMELAB_ACTIONS, never executing a command it
	// receives over the wire.
	payload, err := json.Marshal(map[string]string{"service": service, "command": command})
	if err != nil {
		http.Error(w, "failed to encode request", http.StatusInternalServerError)
		return
	}

	client := &http.Client{Timeout: restartAgentTimeout}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, restartAgentURL(address), bytes.NewReader(payload))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if agentToken != "" {
		req.Header.Set(agentTokenHeader, agentToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "agent unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Relay the agent's status and JSON body verbatim; its error shape
	// (service/status/output) is what the dashboard renders.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Println("Error relaying agent restart response: ", err)
	}
}

// fleetHandler serves the latest cached poll results as JSON. It doesn't
// poll anything itself - pollLoop already keeps fleetCache fresh - so
// requests return immediately instead of waiting on agent network calls.
//
// Tags are stamped in and removed agents are filtered out here, at serve
// time, rather than baked into fleetCache at poll time - so editing a tag
// or removing an agent from the dashboard is reflected on the very next
// request instead of waiting up to one poll interval.
func fleetHandler(w http.ResponseWriter, r *http.Request) {
	fleetCacheMu.RLock()
	results := fleetCache
	fleetCacheMu.RUnlock()

	visible := make([]AgentResponse, 0, len(results))
	for _, res := range results {
		if !agentStore.Has(res.Address) {
			continue
		}
		res.Tag = agentStore.Tag(res.Address)
		visible = append(visible, res)
	}

	writeJSON(w, visible)
}

// writeJSON encodes v as the JSON response body. Callers that need a
// non-200 status must call w.WriteHeader before this.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Println("Error encoding JSON response: ", err)
	}
}

// agentsHandler manages the persisted set of monitored agents: GET lists
// them, POST adds one, PUT updates an existing one's tag, and DELETE
// (?address=...) removes one. Every mutation persists via agentStore
// immediately, so it survives a restart; pollLoop picks up an added or
// removed address on its next cycle, while fleetHandler reflects a tag
// edit or removal on the very next request (see its comment).
func agentsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, agentStore.List())

	case http.MethodPost:
		var body struct {
			Address string `json:"address"`
			Tag     string `json:"tag"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		agent, err := agentStore.Add(body.Address, body.Tag)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, agentstore.ErrExists) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, agent)

	case http.MethodPut:
		var body struct {
			Address string `json:"address"`
			Tag     string `json:"tag"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if err := agentStore.SetTag(body.Address, body.Tag); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, agentstore.ErrNotFound) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodDelete:
		address := r.URL.Query().Get("address")
		if err := agentStore.Remove(address); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, agentstore.ErrNotFound) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		forgetAgent(address)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.Header().Set("Allow", "GET, POST, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// alertsHandler serves the latest cached alerts list as JSON, the same
// cache-only pattern as fleetHandler.
func alertsHandler(w http.ResponseWriter, r *http.Request) {
	alertsCacheMu.RLock()
	alerts := alertsCache
	alertsCacheMu.RUnlock()

	if alerts == nil {
		alerts = []Alert{}
	}
	writeJSON(w, alerts)
}

// checkAlerts feeds each poll result through the debounce tracker, firing a
// Discord notification for any agent, service, or metric that just crossed
// the threshold into a newly confirmed state, and returns every
// currently-confirmed problem as an Alert for the dashboard's aggregated
// alerts view.
func checkAlerts(results []AgentResponse) []Alert {
	var alerts []Alert

	for _, result := range results {
		rememberHostname(result.Address, result.Data)
		name := displayName(result.Address)

		online := result.Err == ""
		if transition := alertTracker.Observe(result.Address, online); transition != "" {
			if transition == "online" {
				sendAlert(fmt.Sprintf(":white_check_mark: %s is back online", name))
			} else {
				sendAlert(fmt.Sprintf(":rotating_light: %s went offline: %s", name, result.Err))
			}
		}
		if confirmedOnline, since, ok := alertTracker.Confirmed(result.Address); ok && !confirmedOnline {
			alerts = append(alerts, Alert{
				Type:    AlertOffline,
				Target:  name,
				Message: fmt.Sprintf("%s is offline: %s", name, result.Err),
				Since:   since,
			})
		}

		// A host that's currently unreachable has no fresh service data, and
		// its services being unreachable is implied by the host alert above
		// - checking them here would just double up the same outage.
		if !online {
			continue
		}

		for _, svc := range result.Data.Services {
			serviceKey := result.Address + "/" + svc.Name
			transition := alertTracker.Observe(serviceKey, svc.Up)
			if transition != "" {
				if transition == "online" {
					sendAlert(fmt.Sprintf(":white_check_mark: %s on %s is back online", svc.Name, name))
				} else {
					sendAlert(fmt.Sprintf(":warning: %s on %s went offline", svc.Name, name))
				}
			}
			if confirmedOnline, since, ok := alertTracker.Confirmed(serviceKey); ok && !confirmedOnline {
				alerts = append(alerts, Alert{
					Type:    AlertService,
					Target:  name,
					Message: fmt.Sprintf("%s on %s is down", svc.Name, name),
					Since:   since,
				})
			}
		}

		alerts = append(alerts, checkMetricThreshold(result.Address, "CPU", result.Data.CPUUsage, cpuThreshold, name)...)
		alerts = append(alerts, checkMetricThreshold(result.Address, "Memory", result.Data.MemoryUsage, memThreshold, name)...)
		alerts = append(alerts, checkMetricThreshold(result.Address, "Disk", result.Data.DiskUsage, diskThreshold, name)...)
	}

	sort.Slice(alerts, func(i, j int) bool { return alerts[i].Since.After(alerts[j].Since) })
	return alerts
}

// checkMetricThreshold debounces one usage metric (CPU/Memory/Disk) through
// the same Tracker used for online/offline state, keyed per-address-and-
// metric so a sustained breach fires once and a sustained recovery fires
// once, instead of alerting on every poll while a host is under load. It
// returns a single-element Alert slice while the breach is still confirmed,
// or nil once it's healthy.
func checkMetricThreshold(address, label string, value, threshold float64, name string) []Alert {
	key := address + "/" + strings.ToLower(label)
	healthy := value < threshold
	transition := alertTracker.Observe(key, healthy)
	if transition != "" {
		if transition == "online" {
			sendAlert(fmt.Sprintf(":white_check_mark: %s usage on %s back to normal: %.1f%%", label, name, value))
		} else {
			sendAlert(fmt.Sprintf(":rotating_light: %s usage on %s is high: %.1f%% (threshold %.0f%%)", label, name, value, threshold))
		}
	}

	if confirmedOnline, since, ok := alertTracker.Confirmed(key); ok && !confirmedOnline {
		return []Alert{{
			Type:    AlertThreshold,
			Target:  name,
			Message: fmt.Sprintf("%s usage on %s is high: %.1f%% (threshold %.0f%%)", label, name, value, threshold),
			Since:   since,
		}}
	}
	return nil
}

// sendAlert posts a human-readable up/down alert to Discord. It is
// fire-and-forget: the notification is dispatched asynchronously and callers
// get no signal about when (or whether) it completes, which is all the
// production path needs.
func sendAlert(message string) {
	go func() {
		if err := discordNotifier.Send(message); err != nil {
			log.Println("Error sending Discord notification: ", err)
		}
	}()
}

func main() {
	passwordHash := os.Getenv("HOMELAB_PASSWORD_HASH")
	if passwordHash == "" {
		log.Fatal("HOMELAB_PASSWORD_HASH must be set (generate one with `go run ./cmd/hashpw`)")
	}
	authStore := auth.NewStore(passwordHash)

	agentsFile := getAgentsFile(os.Getenv("HOMELAB_AGENTS_FILE"))
	store, err := agentstore.Load(agentsFile, serverAddresses)
	if err != nil {
		log.Fatal("failed to load agent store: ", err)
	}
	agentStore = store

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", authStore.LoginHandler)
	mux.HandleFunc("/api/logout", authStore.LogoutHandler)
	mux.HandleFunc("/api/fleet", authStore.RequireAPI(fleetHandler))
	mux.HandleFunc("/api/alerts", authStore.RequireAPI(alertsHandler))
	mux.HandleFunc("/api/agents", authStore.RequireAPI(agentsHandler))
	mux.HandleFunc("POST /api/agents/{id}/restart", authStore.RequireAPI(restartAgentHandler))

	// login.html/js/css must stay reachable without a session, or nobody
	// could ever log in. Registered as exact paths so they win over the "/"
	// catch-all below without exposing the rest of ./web unauthenticated.
	mux.HandleFunc("/login.html", serveFile("web/login.html"))
	mux.HandleFunc("/login.js", serveFile("web/login.js"))
	mux.HandleFunc("/style.css", serveFile("web/style.css"))

	mux.Handle("/", authStore.RequirePage(http.FileServer(http.Dir("web"))))

	pollInterval := getPollInterval()
	log.Println("Polling agents:", agentStore.Addresses(), "every", pollInterval, "(persisted at", agentsFile+")")
	if os.Getenv("DISCORD_WEBHOOK_URL") == "" {
		log.Println("Discord alerts disabled (DISCORD_WEBHOOK_URL not set)")
	} else {
		log.Println("Discord alerts enabled, threshold:", getAlertThreshold(), "consecutive polls")
	}
	log.Printf("Usage thresholds: CPU %.0f%%, Memory %.0f%%, Disk %.0f%%", cpuThreshold, memThreshold, diskThreshold)
	if agentToken != "" {
		log.Println("Sending agent token with every poll (HOMELAB_AGENT_TOKEN set)")
	} else {
		log.Println("No agent token configured - polling agents unauthenticated (set HOMELAB_AGENT_TOKEN to require one)")
	}

	// Poll once synchronously so fleetCache isn't empty for the first
	// request, then hand off to the background loop for the life of the
	// process.
	fleetCache = pollAll(agentStore.Addresses())
	annotateLastSeen(fleetCache)
	annotateHistory(fleetCache)
	alertsCache = checkAlerts(fleetCache)
	go pollLoop(pollInterval)

	// Explicit timeouts: the zero-value http.Server has none, so a client
	// that opens a connection and then stalls mid-request holds a goroutine
	// (and its memory) indefinitely.
	srv := &http.Server{
		Addr:              ":9090",
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Println("Backend starting on :9090")
	log.Fatal(srv.ListenAndServe())
}

func serveFile(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, path)
	}
}

// contentSecurityPolicy is applied to every response (login page, dashboard,
// static assets, and the JSON API alike). script-src has no 'unsafe-inline'
// and the app has no inline <script> or inline event-handler attributes
// (web/register-sw.js and web/app.js are both external files, and all
// interaction is wired up via addEventListener) - so this alone would have
// blocked the inline-attribute XSS payload found in an earlier review, even
// if the escaping fix had a hole. style-src needs 'unsafe-inline' because
// metricRow() (web/app.js) sets an inline style="width:...%" on each
// CPU/memory/disk bar; that's a plain CSS property, not executable, so it's
// a much smaller concession than 'unsafe-inline' on script-src would be.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self'; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// securityHeaders wraps next so every response - not just the ones that
// happen to render agent-supplied data - carries the CSP above.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}
