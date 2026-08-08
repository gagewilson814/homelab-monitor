package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

type Response struct {
	Hostname    string  `json:"hostname"`
	CPUUsage    float64 `json:"cpu_usage"`
	MemoryUsage float64 `json:"memory_usage"`
	DiskUsage   float64 `json:"disk_usage"`
	Uptime      string  `json:"uptime"`
}

func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		log.Println("Error getting hostname: ", err)
		return "unknown"
	}
	return hostname
}

func getMemoryUsage() float64 {
	memory, err := mem.VirtualMemory()
	if err != nil {
		log.Println("Error getting memory usage: ", err)
		return 0.0
	}
	return memory.UsedPercent
}

func getCPUUsage() float64 {
	cpuUsage, err := cpu.Percent(0, false)
	if err != nil {
		log.Println("Error getting CPU usage: ", err)
		return 0.0
	}
	return cpuUsage[0]

}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	response := Response{
		Hostname:    getHostname(),    // Placeholder for actual hostname
		CPUUsage:    getCPUUsage(),    // Placeholder for actual CPU usage
		MemoryUsage: getMemoryUsage(), // Placeholder for actual memory usage
		DiskUsage:   0.0,              // Placeholder for actual disk usage
		Uptime:      "0s",             // Placeholder for actual uptime
	}
	log.Println("Sending response:")
	log.Println(response)
	jsonResponse := json.NewEncoder(w).Encode(response)
	if jsonResponse != nil {
		log.Println("Error encoding JSON response: ", jsonResponse)
	}
}

func main() {
	http.HandleFunc("/stats", homeHandler)
	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
