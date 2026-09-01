package stats

import (
	"encoding/json"
	"testing"
)

// TestResponseWireFormat pins the JSON shape the Agent emits and the Backend
// decodes. This is the contract between the two binaries, so the field names
// must match exactly.
func TestResponseWireFormat(t *testing.T) {
	in := Response{
		Hostname:    "node1",
		CPUUsage:    12.5,
		MemoryUsage: 40.0,
		DiskUsage:   30.0,
		Uptime:      3600,
		Services:    []ServiceStatus{{Name: "jellyfin", Port: 8096, Up: true}},
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, field := range []string{"hostname", "cpu_usage", "memory_usage", "disk_usage", "uptime"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("missing field %q in %s", field, string(data))
		}
	}
	if decoded["hostname"] != "node1" {
		t.Errorf("hostname = %v, want node1", decoded["hostname"])
	}
	svcs, _ := decoded["services"].([]interface{})
	if len(svcs) != 1 || svcs[0].(map[string]interface{})["name"] != "jellyfin" {
		t.Errorf("services = %v, want [{jellyfin}]", svcs)
	}
}

// TestNilServicesOmitted confirms a host with no configured services omits
// the field entirely (omitempty), so the dashboard doesn't render an empty
// array.
func TestNilServicesOmitted(t *testing.T) {
	in := Response{Hostname: "node2", CPUUsage: 0}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(data); contains(got, "services") {
		t.Errorf("expected no services field in %s", string(data))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
