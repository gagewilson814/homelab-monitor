package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"homeserver-monitor/internal/stats"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

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
	response := stats.Response{
		Hostname:    getHostname(),
		CPUUsage:    getCPUUsage(),
		MemoryUsage: getMemoryUsage(),
		DiskUsage:   getDiskUsage(),
		Uptime:      getUptime(),
	}
	log.Println("Sending response:")
	log.Println(response)
	jsonResponse := json.NewEncoder(w).Encode(response)
	if jsonResponse != nil {
		log.Println("Error encoding JSON response: ", jsonResponse)
	}
}

func main() {
	port := os.Getenv("AGENT_PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/stats", homeHandler)
	log.Println("Server starting on :" + port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
