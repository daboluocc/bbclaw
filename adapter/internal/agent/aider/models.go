// aider model catalog. Aider accepts a wide range of LiteLLM-flavoured
// model IDs (anthropic/, openai/, gemini/, ...); we ship a small curated
// subset covering the common cases so the device picker stays scannable.

package aider

import (
	"context"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

var aiderModels = []agent.ModelInfo{
	{ID: "anthropic/claude-sonnet-4-6", Label: "Sonnet 4.6"},
	{ID: "anthropic/claude-opus-4-7", Label: "Opus 4.7"},
	{ID: "openai/gpt-5", Label: "GPT-5"},
	{ID: "openai/gpt-5-mini", Label: "GPT-5 mini"},
	{ID: "deepseek/deepseek-coder", Label: "DeepSeek Coder"},
}

// ListModels implements agent.ModelLister.
func (d *Driver) ListModels(_ context.Context) ([]agent.ModelInfo, error) {
	out := make([]agent.ModelInfo, len(aiderModels))
	copy(out, aiderModels)
	return out, nil
}
