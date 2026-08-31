// Command backend is the central service. It lists the agents to monitor
// (from the HOMELAB_AGENTS env var), polls them all at once with a
// goroutine per agent, aggregates the results, and serves the fleet as JSON
// at /api/fleet. It also serves the static web dashboard from ./web.
package main

import (
	"encoding/json"
	"fmt"
	"homeserver-monitor/internal/auth"
	"homeserver-monitor/internal/stats"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// serverAddresses is the list of agent endpoints to poll. It is resolved
// once at startup from the environment so the HTTP handlers can read it
// without any shared mutable state.
var serverAddresses = getServerAddresses()

// getServerAddresses parses the comma-separated HOMELAB_AGENTS env var into
// a clean, whitespace-trimmed list, skipping any empty entries. It falls
// back to localhost:8080 and localhost:8081 so the backend runs even with
// no agents configured.
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
	if err := json.NewEncoder(w).Encode(results); err != nil {
		log.Println("Error encoding JSON response: ", err)
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

	// Serve the dashboard from ./web for any request not matched above.
	mux.Handle("/", authStore.RequirePage(http.FileServer(http.Dir("web"))))

	log.Println("Polling agents:", serverAddresses)
	log.Println("Backend starting on :9090")
	log.Fatal(http.ListenAndServe(":9090", mux))
}

func serveFile(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, path)
	}
}
