package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrowsersAreSentToTheDocs(t *testing.T) {
	protocol := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	h := browsersToDocs(protocol)

	cases := []struct {
		name, method, accept string
		wantStatus           int
	}{
		{"a person in a browser", http.MethodGet, "text/html,application/xhtml+xml,*/*;q=0.8", http.StatusFound},
		// A client opening the event stream must get the protocol's own answer.
		{"an MCP client's GET", http.MethodGet, "text/event-stream", http.StatusMethodNotAllowed},
		{"a bare GET", http.MethodGet, "", http.StatusMethodNotAllowed},
		// Whatever it says it accepts, a POST is a protocol message.
		{"a POST that also accepts HTML", http.MethodPost, "text/html, application/json", http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "/mcp", nil)
			if c.accept != "" {
				req.Header.Set("Accept", c.accept)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			if c.wantStatus == http.StatusFound && rec.Header().Get("Location") != docsURL {
				t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), docsURL)
			}
		})
	}
}
