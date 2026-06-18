package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerServesIndex verifies the embedded file server returns the static
// client at the site root and at the explicit /index.html path, with an HTML
// content type. These are the two ways a browser reaches the page (a bare visit
// to "/" and a hard link), so both must resolve to the embedded asset.
func TestHandlerServesIndex(t *testing.T) {
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)

	tests := []struct {
		name string
		path string
	}{
		{"root serves index", "/"},
		{"explicit index path", "/index.html"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tt.path)
			if err != nil {
				t.Fatalf("get %s: %v", tt.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Fatalf("content-type = %q, want text/html…", ct)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !strings.Contains(string(body), "<title>bbclaw adapter v2</title>") {
				t.Fatalf("served body is not the bbclaw client (missing title)")
			}
		})
	}
}

// TestHandlerMissingFile confirms a request for a non-existent asset 404s rather
// than falling through to the index — the file server has no SPA fallback, and
// the single-page client needs none (it lives entirely at "/").
func TestHandlerMissingFile(t *testing.T) {
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/does-not-exist.js")
	if err != nil {
		t.Fatalf("get missing asset: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
