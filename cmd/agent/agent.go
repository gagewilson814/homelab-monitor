// Command agent runs on each machine in the fleet. It exposes a single
// /stats HTTP endpoint that reports this machine's hostname, CPU, memory,
// disk and uptime as JSON. gopsutil is used for the metrics and the binary
// is compiled statically so the same source runs on Linux and Windows.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
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
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", svc.Port), 2*time.Second)
	if err != nil {
		return stats.ServiceStatus{Name: svc.Name, Port: svc.Port, Up: false}
	}
	conn.Close()
	return stats.ServiceStatus{Name: svc.Name, Port: svc.Port, Up: true}
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
	if agentToken != "" {
		log.Println("Agent token auth enabled (HOMELAB_AGENT_TOKEN set) - /stats requires a matching", agentTokenHeader, "header")
	} else {
		log.Println("Agent token auth disabled - set HOMELAB_AGENT_TOKEN on both this agent and the backend to require it")
	}
	http.HandleFunc("/stats", homeHandler)

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
