package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatsHandlerRequiresConfiguredToken(t *testing.T) {
	original := agentToken
	agentToken = "test-token"
	t.Cleanup(func() { agentToken = original })

	request := httptest.NewRequest(http.MethodGet, "/stats", nil)
	response := httptest.NewRecorder()
	homeHandler(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}
