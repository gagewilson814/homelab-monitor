// Command backend is the central service. It lists the agents to monitor
// (from the HOMELAB_AGENTS env var), polls them all at once with a
// goroutine per agent, aggregates the results, and serves the fleet as JSON
// at /api/fleet. It also serves the static web dashboard from ./web.
package main

import (
	"encoding/json"
	"fmt"
	"homeserver-monitor/internal/alert"
	"homeserver-monitor/internal/auth"
	"homeserver-monitor/internal/notify"
	"homeserver-monitor/internal/stats"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var serverAddresses = getServerAddresses()

// discordNotifier sends the up/down alerts. Its webhook URL is optional -
// an empty DISCORD_WEBHOOK_URL just makes Send a no-op, so alerting is
// silently disabled rather than requiring its own feature flag.
var discordNotifier = notify.NewDiscord(os.Getenv("DISCORD_WEBHOOK_URL"))

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

// getAlertThreshold reads DISCORD_ALERT_THRESHOLD, defaulting to 2
// consecutive polls (about 10s at the dashboard's 5s refresh interval).
func getAlertThreshold() int {
	if env := os.Getenv("DISCORD_ALERT_THRESHOLD"); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 {
			return n
		}
	}
	return 2
}

func getServerAddresses() []string {
	if env := os.Getenv("HOMELAB_AGENTS"); env != "" {
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
	Address string          `json:"address"`
	Data    *stats.Response `json:"data,omitempty"`
	Err     string          `json:"error,omitempty"`
}

// poll fetches a single agent's /stats endpoint and decodes it. Any of the
// three failure modes (network, non-200 status, bad JSON) are reported via
// Err rather than panicking, so one bad agent can't sink the whole fleet.
func poll(server string) AgentResponse {

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	response, err := client.Get("http://" + server + "/stats")
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

// fleetHandler aggregates every agent and returns the combined list as
// JSON. Ordering matches the configured agent list since each goroutine
// writes to its own index.
func fleetHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	results := pollAll(serverAddresses)
	checkAlerts(results)
	if err := json.NewEncoder(w).Encode(results); err != nil {
		log.Println("Error encoding JSON response: ", err)
	}
}

// checkAlerts feeds each poll result through the debounce tracker and fires
// a Discord notification for any agent that just crossed the threshold into
// a newly confirmed online/offline state. Sends run in their own goroutine
// so a slow or unreachable Discord webhook never delays the dashboard poll.
func checkAlerts(results []AgentResponse) {
	for _, result := range results {
		rememberHostname(result.Address, result.Data)

		transition := alertTracker.Observe(result.Address, result.Err == "")
		if transition == "" {
			continue
		}

		name := result.Address
		if hostname := lookupHostname(result.Address); hostname != "" {
			name = fmt.Sprintf("%s (%s)", hostname, result.Address)
		}

		var message string
		if transition == "online" {
			message = fmt.Sprintf(":white_check_mark: %s is back online", name)
		} else {
			message = fmt.Sprintf(":rotating_light: %s went offline: %s", name, result.Err)
		}

		go func(msg string) {
			if err := discordNotifier.Send(msg); err != nil {
				log.Println("Error sending Discord notification: ", err)
			}
		}(message)
	}
}

func main() {
	passwordHash := os.Getenv("HOMELAB_PASSWORD_HASH")
	if passwordHash == "" {
		log.Fatal("HOMELAB_PASSWORD_HASH must be set (generate one with `go run ./cmd/hashpw`)")
	}
	authStore := auth.NewStore(passwordHash)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", authStore.LoginHandler)
	mux.HandleFunc("/api/logout", authStore.LogoutHandler)
	mux.HandleFunc("/api/fleet", authStore.RequireAPI(fleetHandler))

	// login.html/js/css must stay reachable without a session, or nobody
	// could ever log in. Registered as exact paths so they win over the "/"
	// catch-all below without exposing the rest of ./web unauthenticated.
	mux.HandleFunc("/login.html", serveFile("web/login.html"))
	mux.HandleFunc("/login.js", serveFile("web/login.js"))
	mux.HandleFunc("/style.css", serveFile("web/style.css"))

	mux.Handle("/", authStore.RequirePage(http.FileServer(http.Dir("web"))))

	log.Println("Polling agents:", serverAddresses)
	if os.Getenv("DISCORD_WEBHOOK_URL") == "" {
		log.Println("Discord alerts disabled (DISCORD_WEBHOOK_URL not set)")
	} else {
		log.Println("Discord alerts enabled, threshold:", getAlertThreshold(), "consecutive polls")
	}
	log.Println("Backend starting on :9090")
	log.Fatal(http.ListenAndServe(":9090", mux))
}

func serveFile(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, path)
	}
}
