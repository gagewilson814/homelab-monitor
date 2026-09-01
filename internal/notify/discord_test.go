package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDiscordDisabledWhenUnconfigured confirms an empty webhook URL makes
// Send a no-op (returns nil, posts nothing) rather than erroring.
func TestDiscordDisabledWhenUnconfigured(t *testing.T) {
	d := NewDiscord("")
	if err := d.Send("hello"); err != nil {
		t.Fatalf("Send with empty webhook should be a no-op, got %v", err)
	}
}

func TestDiscordSendsContentToWebhook(t *testing.T) {
	var gotBody webhookPayload
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDiscord(srv.URL)
	if err := d.Send("server is back online"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotPath != "/" {
		t.Errorf("path = %q, want /", gotPath)
	}
	if gotBody.Content != "server is back online" {
		t.Errorf("content = %q, want the message", gotBody.Content)
	}
}

// A hostname or tag containing "@everyone" ends up in an alert's content
// (see displayName in cmd/backend), and Discord parses mentions in content
// by default. Without allowed_mentions restricting that, a monitored
// machine could mass-ping the whole channel just by being named badly.
func TestDiscordSuppressesMentions(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDiscord(srv.URL)
	if err := d.Send("@everyone the NAS is offline"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	allowed, ok := gotBody["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("allowed_mentions missing or wrong shape in payload: %#v", gotBody)
	}
	parse, ok := allowed["parse"].([]any)
	if !ok || len(parse) != 0 {
		t.Errorf("allowed_mentions.parse = %#v, want an empty array", allowed["parse"])
	}
}

func TestDiscordReturnsErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	d := NewDiscord(srv.URL)
	err := d.Send("boom")
	if err == nil {
		t.Fatal("expected an error on a 502 response")
	}
}

func TestDiscordReturnsErrorOnHTTPFailure(t *testing.T) {
	// Point at a port that isn't listening; the client should surface the
	// transport error rather than panicking.
	d := NewDiscord("http://127.0.0.1:1/")
	if err := d.Send("unreachable"); err == nil {
		t.Fatal("expected a transport error for an unreachable webhook")
	}
}

// A webhook URL is itself the credential, and net/http's *url.Error embeds
// the full URL in its message - which the backend then writes to the log.
func TestDiscordErrorDoesNotLeakWebhookURL(t *testing.T) {
	const secret = "s3cret-webhook-token"
	d := NewDiscord("http://127.0.0.1:1/api/webhooks/12345/" + secret)

	err := d.Send("unreachable")
	if err == nil {
		t.Fatal("expected a transport error for an unreachable webhook")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the webhook token: %q", err.Error())
	}
}
