package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerServesIndex verifies the embedded handler returns the built SPA at
// the site root and at an explicit /index.html path, with an HTML content type.
// These are the two ways a browser reaches the shell (a bare visit to "/" and a
// hard link), so both must resolve to the embedded index.html.
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
			// The built SPA shell mounts into <div id="app"> — a stable marker
			// across rebuilds (the script/asset names are hashed).
			if !strings.Contains(string(body), `id="app"`) {
				t.Fatalf("served body is not the SPA shell (missing #app mount)")
			}
		})
	}
}

// TestHandlerSPAFallback confirms an unknown path falls back to the SPA shell
// (index.html, 200) rather than 404ing, so client-side routes / deep links
// always load the app.
func TestHandlerSPAFallback(t *testing.T) {
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/some/client/route")
	if err != nil {
		t.Fatalf("get client route: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SPA fallback)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `id="app"`) {
		t.Fatalf("fallback body is not the SPA shell")
	}
}

// TestHandlerServesHashedAsset confirms a real built asset is served with the
// correct content type (not the HTML fallback) — i.e. existing dist files win
// over the fallback.
func TestHandlerServesHashedAsset(t *testing.T) {
	srv := httptest.NewServer(Handler())
	t.Cleanup(srv.Close)

	// Pull the asset path out of the shell so the test isn't pinned to a
	// specific content hash.
	idx, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get index: %v", err)
	}
	shell, _ := io.ReadAll(idx.Body)
	idx.Body.Close()

	const marker = `/assets/`
	i := strings.Index(string(shell), marker)
	if i < 0 {
		t.Skip("no hashed asset referenced in shell; nothing to check")
	}
	rest := string(shell)[i:]
	end := strings.IndexAny(rest, `"'`)
	if end < 0 {
		t.Fatalf("could not parse asset path from shell")
	}
	assetPath := rest[:end]

	resp, err := http.Get(srv.URL + assetPath)
	if err != nil {
		t.Fatalf("get asset %s: %v", assetPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset %s status = %d, want 200", assetPath, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		t.Fatalf("asset %s served as HTML (fallback leaked): %q", assetPath, ct)
	}
}
