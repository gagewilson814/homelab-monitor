package main

import (
	"encoding/json"
	"fmt"
	"homeserver-monitor/internal/stats"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var serverAddresses = getServerAddresses()

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

type AgentResponse struct {
	Address string          `json:"address"`
	Data    *stats.Response `json:"data,omitempty"`
	Err     string          `json:"error,omitempty"`
}

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

func fleetHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	results := pollAll(serverAddresses)
	if err := json.NewEncoder(w).Encode(results); err != nil {
		log.Println("Error encoding JSON response: ", err)
	}
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/fleet", fleetHandler)
	mux.Handle("/", http.FileServer(http.Dir("web")))

	log.Println("Polling agents:", serverAddresses)
	log.Println("Backend starting on :9090")
	log.Fatal(http.ListenAndServe(":9090", mux))
}
