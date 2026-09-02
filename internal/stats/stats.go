// Package stats defines the shared data types used across the homelab
// monitor. Both the Agent and the Backend reference stats.Response so the
// JSON shape stays consistent between the machine that collects the metrics
// and the machine that aggregates and serves them.
package stats

// Response is the payload returned by an Agent's /stats endpoint. The json
// tags define the wire format that the Backend decodes and the dashboard
// renders. Percentages are reported as 0-100 with one decimal place (e.g.
// 42.5), and uptime is a raw second count that the frontend formats.
type Response struct {
	Hostname    string          `json:"hostname"`
	CPUUsage    float64         `json:"cpu_usage"`
	MemoryUsage float64         `json:"memory_usage"`
	DiskUsage   float64         `json:"disk_usage"`
	Uptime      uint64          `json:"uptime"`
	Services    []ServiceStatus `json:"services,omitempty"`
}

// ServiceStatus is a single service-level check result, e.g. whether
// Jellyfin's port is answering on the machine an Agent runs on. Configured
// per-Agent via HOMELAB_SERVICES; a host can be up while one of its
// services is not.
type ServiceStatus struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Up   bool   `json:"up"`
	// Action is the shell command the Agent will run when asked to restart
	// this service (from HOMELAB_ACTIONS on the agent machine). Empty means
	// no restart is configured and the dashboard shows no restart button.
	// It's agent-side config relayed for display only - the backend and the
	// dashboard never get to choose what runs.
	Action string `json:"action,omitempty"`
}
