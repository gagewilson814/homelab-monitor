// Package notify sends outbound alert messages.
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Notifier is the interface the backend depends on for sending alerts. It is
// satisfied by *Discord and by test doubles, which is what lets alert paths
// be exercised without a real webhook.
type Notifier interface {
	Send(content string) error
}

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

// webhookPayload is the JSON body posted to the Discord webhook.
type webhookPayload struct {
	Content string `json:"content"`
	// AllowedMentions restricts which mentions in Content actually notify
	// anyone. Discord parses @everyone/@here/role mentions in content by
	// default when this is omitted - and content is built from agent-
	// supplied data (hostname) and user-supplied data (a tag), either of
	// which could contain "@everyone" and mass-ping the channel on every
	// alert. An empty Parse list means "resolve no mentions at all".
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

type allowedMentions struct {
	Parse []string `json:"parse"`
}

func (d *Discord) Send(content string) error {
	if d.webhookURL == "" {
		return nil
	}

	body, err := json.Marshal(webhookPayload{
		Content:         content,
		AllowedMentions: allowedMentions{Parse: []string{}},
	})
	if err != nil {
		return err
	}

	resp, err := d.client.Post(d.webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		// net/http wraps failures in *url.Error, whose Error() embeds the
		// full URL - and a Discord webhook URL is itself the credential.
		// Callers log this error, so unwrap to keep the token out of logs.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("discord webhook request failed: %w", urlErr.Err)
		}
		return fmt.Errorf("discord webhook request failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("discord webhook returned status %s", resp.Status)
	}
	return nil
}
