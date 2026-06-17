package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// ModelLister + SessionLister for the serve driver. Both hit plain JSON GET
// endpoints with stable shapes, so they use raw HTTP rather than the SDK —
// skew-proof and dependency-light (ADR-031).

// ListModels enumerates authed providers and their models from
// GET /config/providers, returning IDs in "provider/model" form (the shape
// StartOpts.Model / splitModel expect). Replaces the hand-maintained models.go.
func (d *ServeDriver) ListModels(ctx context.Context) ([]agent.ModelInfo, error) {
	if err := d.ensureReady(); err != nil {
		return nil, err
	}
	var body struct {
		Providers []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Key    string `json:"key"`
			Models map[string]struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := d.getJSON(ctx, "/config/providers", &body); err != nil {
		return nil, err
	}
	var out []agent.ModelInfo
	for _, p := range body.Providers {
		// Only surface providers the user has actually authed — an unauthed
		// provider's models cannot be driven and would just clutter the picker.
		if p.Key == "" {
			continue
		}
		for _, m := range p.Models {
			label := m.Name
			if label == "" {
				label = m.ID
			}
			out = append(out, agent.ModelInfo{
				ID:    p.ID + "/" + m.ID,
				Label: label,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListSessions enumerates persisted opencode sessions from GET /session.
func (d *ServeDriver) ListSessions(ctx context.Context, limit int) ([]agent.SessionInfo, error) {
	if err := d.ensureReady(); err != nil {
		return nil, err
	}
	var sessions []struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Slug    string `json:"slug"`
		Version string `json:"version"`
		Time    struct {
			Created int64 `json:"created"`
			Updated int64 `json:"updated"`
		} `json:"time"`
	}
	if err := d.getJSON(ctx, "/session", &sessions); err != nil {
		return nil, err
	}
	// Newest first.
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Time.Updated > sessions[j].Time.Updated })
	out := make([]agent.SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		if limit > 0 && len(out) >= limit {
			break
		}
		preview := firstNonEmptyStr(s.Title, s.Slug, s.ID)
		out = append(out, agent.SessionInfo{
			ID:       s.ID,
			Preview:  preview,
			LastUsed: s.Time.Updated / 1000, // ms → Unix seconds
		})
	}
	return out, nil
}

// CLISessionExists validates a resume target before spawning work, avoiding a
// cold-start that would immediately fail (ADR cross-ref: CLISessionChecker).
func (d *ServeDriver) CLISessionExists(cliSessionID string) bool {
	if cliSessionID == "" {
		return false
	}
	if err := d.ensureReady(); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var s struct {
		ID string `json:"id"`
	}
	if err := d.getJSON(ctx, "/session/"+cliSessionID, &s); err != nil {
		return false
	}
	return s.ID != ""
}

func (d *ServeDriver) getJSON(ctx context.Context, path string, dst any) error {
	d.mu.Lock()
	base := d.baseURL
	d.mu.Unlock()
	if base == "" {
		return fmt.Errorf("opencode serve: not started")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("opencode serve: GET %s → %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

var (
	_ agent.ModelLister       = (*ServeDriver)(nil)
	_ agent.SessionLister     = (*ServeDriver)(nil)
	_ agent.CLISessionChecker = (*ServeDriver)(nil)
)
