// claudecode model catalog. Surfaced to the device through the optional
// agent.ModelLister interface so the settings UI can render a list.
//
// This list is hand-maintained: Anthropic doesn't publish a stable
// machine-readable catalog endpoint for the model IDs the `claude` CLI
// accepts, and the CLI itself doesn't have a `--list-models` flag we can
// shell out to. Adding/removing a model is a one-line edit here.
//
// The Label is what the device shows; the ID is what gets passed as
// `--model <id>` to the CLI when the user picks this row.

package claudecode

import (
	"context"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// claudeCodeModels is the static, ordered catalogue of selectable models.
// First entry is treated as the implicit "factory default" if no operator
// override exists.
var claudeCodeModels = []agent.ModelInfo{
	{ID: "claude-sonnet-4-6", Label: "Sonnet 4.6"},
	{ID: "claude-opus-4-7", Label: "Opus 4.7"},
	{ID: "claude-haiku-4-5", Label: "Haiku 4.5"},
}

// defaultModelID is the implicit factory default: the first catalog entry.
// The driver uses it as the --model fallback when no operator/user override is
// configured, so the runtime default and the device's "factory default" model
// list never drift (they used to: a stale hardcoded claude-sonnet-4-5 that
// wasn't even in this catalog). Empty only when the catalog itself is empty.
func defaultModelID() string {
	if len(claudeCodeModels) == 0 {
		return ""
	}
	return claudeCodeModels[0].ID
}

// ListModels implements agent.ModelLister.
func (d *Driver) ListModels(_ context.Context) ([]agent.ModelInfo, error) {
	out := make([]agent.ModelInfo, len(claudeCodeModels))
	copy(out, claudeCodeModels)
	return out, nil
}
