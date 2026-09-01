package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	var gotBody map[string]string
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
	if gotBody["content"] != "server is back online" {
		t.Errorf("content = %q, want the message", gotBody["content"])
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
