// opencode model catalog. See claudecode/models.go for the rationale on
// keeping this as a static hand-maintained list.

package opencode

import (
	"context"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// opencodeModels is the catalogue presented to the device. IDs match what
// the `opencode run --model <id>` CLI accepts (provider/model form).
var opencodeModels = []agent.ModelInfo{
	{ID: "anthropic/claude-sonnet-4-6", Label: "Sonnet 4.6"},
	{ID: "anthropic/claude-opus-4-7", Label: "Opus 4.7"},
	{ID: "anthropic/claude-haiku-4-5", Label: "Haiku 4.5"},
	{ID: "openai/gpt-5", Label: "GPT-5"},
	{ID: "openai/gpt-5-mini", Label: "GPT-5 mini"},
	{ID: "deepseek/deepseek-v4-pro", Label: "DeepSeek v4 Pro"},
}

// ListModels implements agent.ModelLister.
func (d *Driver) ListModels(_ context.Context) ([]agent.ModelInfo, error) {
	out := make([]agent.ModelInfo, len(opencodeModels))
	copy(out, opencodeModels)
	return out, nil
}
