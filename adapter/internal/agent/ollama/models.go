// Dynamic model catalog for ollama. Calls GET /api/tags on the local
// Ollama server and translates the response into agent.ModelInfo entries.
//
// We cache the result for 60 seconds so that repeated calls from the
// device settings screen don't hammer the local server, but a freshly
// pulled model still shows up within a minute without an adapter restart.
//
// On any failure (network, non-200, parse error) we return the last
// successful snapshot if one exists, otherwise an empty list. Callers
// never see an error — the device UI gracefully degrades to "no models"
// when the local Ollama server is unreachable.

package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

const (
	tagsTTL     = 60 * time.Second
	tagsTimeout = 2 * time.Second
)

type tagsCache struct {
	mu      sync.Mutex
	at      time.Time
	models  []agent.ModelInfo
}

var sharedTagsCache tagsCache

// tagsResponse matches GET /api/tags:
//
//	{"models":[{"name":"llama3.1:8b","modified_at":"...","size":...,...}, ...]}
type tagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// ListModels implements agent.ModelLister. See package docstring for caching
// + error-handling semantics.
func (d *Driver) ListModels(ctx context.Context) ([]agent.ModelInfo, error) {
	sharedTagsCache.mu.Lock()
	fresh := !sharedTagsCache.at.IsZero() && time.Since(sharedTagsCache.at) < tagsTTL
	cached := append([]agent.ModelInfo(nil), sharedTagsCache.models...)
	sharedTagsCache.mu.Unlock()

	if fresh {
		return cached, nil
	}

	tctx, cancel := context.WithTimeout(ctx, tagsTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(tctx, http.MethodGet, d.baseURL+"/api/tags", nil)
	if err != nil {
		return cached, nil
	}
	resp, err := d.http.Do(req)
	if err != nil {
		d.log.Warnf("ollama: ListModels: %v (returning %d cached)", err, len(cached))
		return cached, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		d.log.Warnf("ollama: ListModels: http %d (returning %d cached)", resp.StatusCode, len(cached))
		return cached, nil
	}
	var body tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		d.log.Warnf("ollama: ListModels: decode: %v (returning %d cached)", err, len(cached))
		return cached, nil
	}
	out := make([]agent.ModelInfo, 0, len(body.Models))
	for _, m := range body.Models {
		if m.Name == "" {
			continue
		}
		out = append(out, agent.ModelInfo{ID: m.Name, Label: m.Name})
	}

	sharedTagsCache.mu.Lock()
	sharedTagsCache.at = time.Now()
	sharedTagsCache.models = out
	sharedTagsCache.mu.Unlock()

	if len(out) == 0 {
		// Empty list from a healthy ollama means "no models pulled yet" —
		// surface a synthetic hint so the device row isn't blank.
		return []agent.ModelInfo{{ID: "", Label: fmt.Sprintf("(no models — pull one with `ollama pull`)")}}, nil
	}
	return out, nil
}
