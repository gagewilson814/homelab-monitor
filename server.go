package main

import (
	"encoding/json"
	"log"
	"net/http"
)

type Response struct {
	Hostname    string  `json:"hostname"`
	CPUUsage    float64 `json:"cpu_usage"`
	MemoryUsage float64 `json:"memory_usage"`
	DiskUsage   float64 `json:"disk_usage"`
	Uptime      string  `json:"uptime"`
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	response := Response{
		Hostname:    "localhost", // Placeholder for actual hostname
		CPUUsage:    0.0,         // Placeholder for actual CPU usage
		MemoryUsage: 0.0,         // Placeholder for actual memory usage
		DiskUsage:   0.0,         // Placeholder for actual disk usage
		Uptime:      "0s",        // Placeholder for actual uptime
	}
	log.Println("Sending response:")
	log.Println(response)
	log.Fatal(json.NewEncoder(w).Encode(response))
}

func main() {
	http.HandleFunc("/stats", homeHandler)
	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
