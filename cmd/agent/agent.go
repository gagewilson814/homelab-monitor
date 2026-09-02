// Command agent runs on each machine in the fleet. It exposes a single
// /stats HTTP endpoint that reports this machine's hostname, CPU, memory,
// disk and uptime as JSON. gopsutil is used for the metrics and the binary
// is compiled statically so the same source runs on Linux and Windows.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"homeserver-monitor/internal/stats"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

// configuredServices lists which local services to check alongside the
// standard host metrics. Parsed once at startup from HOMELAB_SERVICES.
var configuredServices = getConfiguredServices()

// agentTokenHeader is the header the backend's poll() sends its shared
// secret in - the two names must match, since they're the same string
// literal maintained independently in cmd/backend/backend.go.
const agentTokenHeader = "X-Homelab-Agent-Token"

// agentToken is the shared secret /stats requires, if any. Left unset (the
// default), homeHandler serves every request exactly as it did before this
// existed - opting in requires setting HOMELAB_AGENT_TOKEN here *and* on
// the backend that polls this agent.
var agentToken = os.Getenv("HOMELAB_AGENT_TOKEN")

// configuredActions maps a service name to the shell command that restarts
// it, parsed once at startup from HOMELAB_ACTIONS. Commands come from the
// operator's own env config on the agent machine - never from the network -
// so there is no injection surface: request input only ever selects a key
// of this map.
var configuredActions = getConfiguredActions()

// restartTimeout caps how long a restart command may run before its context
// is cancelled and the process killed. A var (not a const) so tests can
// shorten it instead of waiting out the full 60s.
var restartTimeout = 60 * time.Second

// restartResponse is the JSON body /restart replies with on both success
// and failure; Status distinguishes them ("ok" vs "error").
type restartResponse struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Output  string `json:"output,omitempty"`
}

// getConfiguredActions parses HOMELAB_ACTIONS as comma-separated
// name:command pairs (e.g. "jellyfin:systemctl restart jellyfin"). The
// command itself may contain colons, so only the FIRST colon separates name
// from command. Malformed entries are skipped with a log line rather than
// failing startup, mirroring getConfiguredServices.
func getConfiguredActions() map[string]string {
	env := os.Getenv("HOMELAB_ACTIONS")
	if env == "" {
		return nil
	}

	actions := make(map[string]string)
	for _, entry := range strings.Split(env, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		name, command, found := strings.Cut(entry, ":")
		name = strings.TrimSpace(name)
		command = strings.TrimSpace(command)
		if !found || name == "" || command == "" {
			log.Printf("Skipping invalid HOMELAB_ACTIONS entry %q: expected name:command", entry)
			continue
		}
		actions[name] = command
	}
	return actions
}

// validAgentToken reports whether a request's token header is acceptable:
// always true when agentToken is unset, otherwise a constant-time compare
// against what the caller sent, so response timing can't be used to guess
// the token byte by byte.
func validAgentToken(got string) bool {
	if agentToken == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(agentToken)) == 1
}

type serviceConfig struct {
	Name string
	Port int
}

// getConfiguredServices parses HOMELAB_SERVICES as comma-separated
// name:port pairs (e.g. "jellyfin:8096,plex:32400"). Malformed entries are
// skipped with a log line rather than failing startup.
func getConfiguredServices() []serviceConfig {
	env := os.Getenv("HOMELAB_SERVICES")
	if env == "" {
		return nil
	}

	var services []serviceConfig
	for _, entry := range strings.Split(env, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		name, portStr, found := strings.Cut(entry, ":")
		if !found {
			log.Printf("Skipping invalid HOMELAB_SERVICES entry %q: expected name:port", entry)
			continue
		}

		port, err := strconv.Atoi(strings.TrimSpace(portStr))
		if err != nil {
			log.Printf("Skipping invalid HOMELAB_SERVICES entry %q: %v", entry, err)
			continue
		}

		services = append(services, serviceConfig{Name: strings.TrimSpace(name), Port: port})
	}
	return services
}

// checkService dials the service's port on localhost with a short timeout.
// A successful TCP connect counts as "up" without inspecting the payload,
// which keeps this generic across arbitrary services.
func checkService(svc serviceConfig) stats.ServiceStatus {
	status := stats.ServiceStatus{Name: svc.Name, Port: svc.Port, Up: false, Action: configuredActions[svc.Name]}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", svc.Port), 2*time.Second)
	if err != nil {
		return status
	}
	conn.Close()
	status.Up = true
	return status
}

// checkServices runs every configured check concurrently, one goroutine per
// service, mirroring the Backend's own polling pattern.
func checkServices(services []serviceConfig) []stats.ServiceStatus {
	if len(services) == 0 {
		return nil
	}

	results := make([]stats.ServiceStatus, len(services))
	var wg sync.WaitGroup
	for i, svc := range services {
		wg.Add(1)
		go func(i int, svc serviceConfig) {
			defer wg.Done()
			results[i] = checkService(svc)
		}(i, svc)
	}
	wg.Wait()

	return results
}

func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		log.Println("Error getting hostname: ", err)
		return "unknown"
	}
	return hostname
}

func getDiskUsage() float64 {
	diskUsage, err := disk.Usage("/")
	if err != nil {
		log.Println("Error getting disk usage: ", err)
		return 0.0
	}
	return diskUsage.UsedPercent
}

func getMemoryUsage() float64 {
	memory, err := mem.VirtualMemory()
	if err != nil {
		log.Println("Error getting memory usage: ", err)
		return 0.0
	}
	return memory.UsedPercent
}

func getUptime() uint64 {
	uptimeSeconds, err := host.Uptime()
	if err != nil {
		log.Println("Error getting uptime: ", err)
		return 0
	}
	return uptimeSeconds
}

// getCPUUsage returns instantaneous CPU utilization as a percentage. The
// Percent call with a 0 duration samples the running load across all cores;
// the result comes back as a slice (one entry per core) so we take [0]. The
// length check matters: gopsutil can return an empty slice with a nil error
// (e.g. when the platform reports no CPU times), and indexing that would
// panic the whole agent rather than degrading to 0 like every other metric.
func getCPUUsage() float64 {
	cpuUsage, err := cpu.Percent(0, false)
	if err != nil {
		log.Println("Error getting CPU usage: ", err)
		return 0.0
	}
	if len(cpuUsage) == 0 {
		log.Println("No CPU usage samples returned")
		return 0.0
	}
	return cpuUsage[0]
}

// homeHandler builds the stats payload and writes it as JSON to the client.
// Every metric is gathered independently and defaults to 0 on error so a
// single failing sensor never breaks the whole response.
// actionNames lists a actions map's keys (sorted for stable log output).
func actionNames(actions map[string]string) []string {
	names := make([]string, 0, len(actions))
	for name := range actions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	if !validAgentToken(r.Header.Get(agentTokenHeader)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	response := stats.Response{
		Hostname:    getHostname(),
		CPUUsage:    getCPUUsage(),
		MemoryUsage: getMemoryUsage(),
		DiskUsage:   getDiskUsage(),
		Uptime:      getUptime(),
		Services:    checkServices(configuredServices),
	}
	log.Println("Sending response:")
	log.Println(response)
	jsonResponse := json.NewEncoder(w).Encode(response)
	if jsonResponse != nil {
		log.Println("Error encoding JSON response: ", jsonResponse)
	}
}

// restartHandler runs the configured restart command for one service.
// The request body selects WHICH configured command runs - it is a map key
// lookup, never a shell string - so a caller can only trigger commands the
// operator put in HOMELAB_ACTIONS on this machine. Guarded by the same
// agent token as /stats when HOMELAB_AGENT_TOKEN is set.
func restartHandler(w http.ResponseWriter, r *http.Request) {
	if !validAgentToken(r.Header.Get(agentTokenHeader)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Service string `json:"service"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(body.Service)
	command, ok := configuredActions[name]
	if !ok {
		http.Error(w, "no restart action configured for service", http.StatusNotFound)
		return
	}

	output, err := runRestartCommand(r.Context(), command)
	if err != nil {
		log.Printf("Restart of %q failed: %v (output: %s)", name, err, output)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(restartResponse{Service: name, Status: "error", Output: output})
		return
	}

	log.Printf("Restart of %q succeeded", name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(restartResponse{Service: name, Status: "ok", Output: output})
}

// runRestartCommand executes command through the platform shell with a
// restartTimeout context, returning its combined output. The shell wrapper
// is what lets an operator write a pipeline or multiple statements as one
// action; the command text itself only ever originates from env config.
func runRestartCommand(parent context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, restartTimeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("restart command timed out after %s", restartTimeout)
	}
	return string(out), err
}

func main() {
	// The port is configurable so several agents can run on one host during
	// testing; it defaults to :8080 to match the documented fleet layout.
	port := os.Getenv("AGENT_PORT")
	if port == "" {
		port = "8080"
	}
	if len(configuredServices) > 0 {
		log.Println("Checking services:", configuredServices)
	}
	if len(configuredActions) > 0 {
		log.Println("Restart actions configured:", actionNames(configuredActions))
	}
	if agentToken != "" {
		log.Println("Agent token auth enabled (HOMELAB_AGENT_TOKEN set) - /stats and /restart require a matching", agentTokenHeader, "header")
	} else {
		log.Println("Agent token auth disabled - set HOMELAB_AGENT_TOKEN on both this agent and the backend to require it")
	}
	http.HandleFunc("/stats", homeHandler)
	http.HandleFunc("/restart", restartHandler)

	// Explicit timeouts; the zero-value http.Server has none, so a stalled
	// client would tie up a goroutine for as long as it likes.
	srv := &http.Server{
		Addr:              ":" + port,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Println("Server starting on :" + port)
	log.Fatal(srv.ListenAndServe())
}
