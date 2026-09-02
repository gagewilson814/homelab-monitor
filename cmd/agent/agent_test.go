package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// withActions swaps the package-level configuredActions map for the duration
// of fn, restoring the original afterwards, so tests don't depend on the
// ambient HOMELAB_ACTIONS env (which is parsed once at init).
func withActions(t *testing.T, actions map[string]string, fn func()) {
	t.Helper()
	orig := configuredActions
	configuredActions = actions
	defer func() { configuredActions = orig }()
	fn()
}

func TestGetConfiguredActionsParsing(t *testing.T) {
	t.Setenv("HOMELAB_ACTIONS", " jellyfin:systemctl restart jellyfin , bad , plex:docker restart plex:latest , empty:, :nocmd ")

	// The first colon splits name from command, so commands containing
	// colons (image tags, drive letters) survive intact.
	got := getConfiguredActions()
	want := map[string]string{
		"jellyfin": "systemctl restart jellyfin",
		"plex":     "docker restart plex:latest",
	}
	if len(got) != len(want) {
		t.Fatalf("getConfiguredActions() = %v, want %v", got, want)
	}
	for name, cmd := range want {
		if got[name] != cmd {
			t.Errorf("action[%q] = %q, want %q", name, got[name], cmd)
		}
	}
}

func TestGetConfiguredActionsEmpty(t *testing.T) {
	t.Setenv("HOMELAB_ACTIONS", "")
	if got := getConfiguredActions(); got != nil {
		t.Errorf("getConfiguredActions() with empty env = %v, want nil", got)
	}
}

func TestCheckServiceSetsAction(t *testing.T) {
	withActions(t, map[string]string{"jellyfin": "systemctl restart jellyfin"}, func() {
		// Port 1 is never listening; the point is that Action is stamped
		// whether the service is up or down.
		svc := checkService(serviceConfig{Name: "jellyfin", Port: 1})
		if svc.Action != "systemctl restart jellyfin" {
			t.Errorf("checkService action = %q, want the configured command", svc.Action)
		}
		if svc.Up {
			t.Error("checkService reported port 1 as up")
		}
	})
}

func TestRestartHandlerRunsConfiguredCommand(t *testing.T) {
	withActions(t, map[string]string{"echo-svc": "printf hello-from-agent"}, func() {
		body := strings.NewReader(`{"service":"echo-svc"}`)
		rr := httptest.NewRecorder()
		restartHandler(rr, httptest.NewRequest(http.MethodPost, "/restart", body))

		if rr.Code != http.StatusOK {
			t.Fatalf("restart code = %d, want 200, body=%s", rr.Code, rr.Body.String())
		}
		var got restartResponse
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Status != "ok" || got.Service != "echo-svc" {
			t.Fatalf("restart response = %+v", got)
		}
		if got.Output != "hello-from-agent" {
			t.Errorf("output = %q, want hello-from-agent", got.Output)
		}
	})
}

func TestRestartHandlerUnknownService(t *testing.T) {
	withActions(t, map[string]string{"jellyfin": "true"}, func() {
		rr := httptest.NewRecorder()
		restartHandler(rr, httptest.NewRequest(http.MethodPost, "/restart", strings.NewReader(`{"service":"nope"}`)))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("unknown service code = %d, want 404", rr.Code)
		}
	})
}

func TestRestartHandlerCommandFailureIsReported(t *testing.T) {
	withActions(t, map[string]string{"fail-svc": "exit 3"}, func() {
		rr := httptest.NewRecorder()
		restartHandler(rr, httptest.NewRequest(http.MethodPost, "/restart", strings.NewReader(`{"service":"fail-svc"}`)))
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("failing command code = %d, want 500", rr.Code)
		}
		var got restartResponse
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Status != "error" {
			t.Errorf("status = %q, want error", got.Status)
		}
	})
}

func TestRestartHandlerRequiresTokenWhenSet(t *testing.T) {
	orig := agentToken
	agentToken = "s3cret"
	defer func() { agentToken = orig }()

	withActions(t, map[string]string{"jellyfin": "true"}, func() {
		req := httptest.NewRequest(http.MethodPost, "/restart", strings.NewReader(`{"service":"jellyfin"}`))

		rr := httptest.NewRecorder()
		restartHandler(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("missing token code = %d, want 401", rr.Code)
		}

		req.Header.Set(agentTokenHeader, "wrong")
		rr = httptest.NewRecorder()
		restartHandler(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("wrong token code = %d, want 401", rr.Code)
		}

		req.Header.Set(agentTokenHeader, "s3cret")
		rr = httptest.NewRecorder()
		restartHandler(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("valid token code = %d, want 200, body=%s", rr.Code, rr.Body.String())
		}
	})
}

func TestRestartHandlerNoTokenRequiredByDefault(t *testing.T) {
	orig := agentToken
	agentToken = ""
	defer func() { agentToken = orig }()

	withActions(t, map[string]string{"jellyfin": "true"}, func() {
		rr := httptest.NewRecorder()
		restartHandler(rr, httptest.NewRequest(http.MethodPost, "/restart", strings.NewReader(`{"service":"jellyfin"}`)))
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 when no token is configured", rr.Code)
		}
	})
}

func TestRestartHandlerMethodNotAllowed(t *testing.T) {
	rr := httptest.NewRecorder()
	restartHandler(rr, httptest.NewRequest(http.MethodGet, "/restart", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code = %d, want 405", rr.Code)
	}
}

func TestRestartHandlerBadBody(t *testing.T) {
	rr := httptest.NewRecorder()
	restartHandler(rr, httptest.NewRequest(http.MethodPost, "/restart", strings.NewReader("{not json")))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad body code = %d, want 400", rr.Code)
	}
}

func TestRunRestartCommandTimesOut(t *testing.T) {
	// Only meaningful on Unix; skip where "sleep" isn't available.
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	orig := restartTimeout
	restartTimeout = 100 * time.Millisecond
	defer func() { restartTimeout = orig }()

	_, err := runRestartCommand(t.Context(), "sleep 5")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("runRestartCommand err = %v, want a timeout error", err)
	}
}
