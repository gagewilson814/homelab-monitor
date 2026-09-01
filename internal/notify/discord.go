// Package notify sends outbound alert messages.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Discord posts messages to a Discord webhook. A zero-value webhookURL
// makes Send a no-op, so notifications are simply disabled when unconfigured
// rather than requiring a feature flag.
type Discord struct {
	webhookURL string
	client     *http.Client
}

func NewDiscord(webhookURL string) *Discord {
	return &Discord{
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: 5 * time.Second},
	}
}

func (d *Discord) Send(content string) error {
	if d.webhookURL == "" {
		return nil
	}

	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return err
	}

	resp, err := d.client.Post(d.webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("discord webhook returned status %s", resp.Status)
	}
	return nil
}
