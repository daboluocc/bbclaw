package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// maxThinkingBytes caps a single thinking block. Thinking can be long; the
// admin page collapses it by default, but we still bound the payload so a
// runaway reasoning trace doesn't bloat the response (ADR-029 §2.2).
const maxThinkingBytes = 8192

// LoadParts implements agent.PartLoader by re-scanning the same JSONL
// transcript LoadMessages reads, but preserving the per-turn structure:
// thinking / text / tool / dispatch blocks in the order they occurred
// (ADR-029 §2.1). Dispatch tool_use blocks are matched to their tool_result
// (by tool_use_id, which may land in a later "user" row) to recover the
// async status/elapsed/error.
//
// Pagination mirrors LoadMessages: `before` is the upper-exclusive seq cursor
// over visible turns; before<=0 means the latest page.
func (d *Driver) LoadParts(ctx context.Context, sid string, before, limit int) (agent.PartsPage, error) {
	if strings.TrimSpace(sid) == "" {
		return agent.PartsPage{}, fmt.Errorf("claude-code: empty session id")
	}
	if limit <= 0 {
		limit = 50
	}

	historyPath, err := d.findHistoryPath(sid)
	if err != nil {
		return agent.PartsPage{}, err
	}
	if historyPath == "" {
		return agent.PartsPage{Turns: []agent.Turn{}}, nil
	}

	turns, err := readAllTurns(historyPath)
	if err != nil {
		return agent.PartsPage{}, fmt.Errorf("claude-code: read transcript %s: %w", historyPath, err)
	}

	total := len(turns)
	if total == 0 {
		return agent.PartsPage{Turns: []agent.Turn{}}, nil
	}

	var end int
	if before <= 0 || before > total {
		end = total
	} else {
		end = before
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	if start >= end {
		return agent.PartsPage{Turns: []agent.Turn{}, Total: total, HasMore: false}, nil
	}

	return agent.PartsPage{
		Turns:   turns[start:end],
		Total:   total,
		HasMore: start > 0,
	}, nil
}

// jsonlRow is the subset of a transcript line we parse for structured parts.
type jsonlRow struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// rowContent flattens message.content into ordered blocks. content can be a
// bare string (user) or an array of typed blocks (assistant / tool_result).
type rowContent struct {
	Content json.RawMessage `json:"content"`
}

// readAllTurns parses the JSONL transcript into visible Turns. tool_result-only
// "user" rows are not visible turns — they fold into the dispatch part of the
// originating assistant turn. Seq is the visible-turn index (pagination cursor).
func readAllTurns(path string) ([]agent.Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Pass 1: collect dispatch tool_result payloads keyed by tool_use_id, so an
	// assistant dispatch block can recover its result even though the result
	// lands in a later "user" row.
	rows, err := scanRows(f)
	if err != nil {
		return nil, err
	}
	dispatchResults := collectDispatchResults(rows)

	// Pass 2: build visible turns.
	var turns []agent.Turn
	for _, row := range rows {
		role := strings.ToLower(strings.TrimSpace(row.Type))
		if role != "user" && role != "assistant" {
			continue
		}
		blocks := decodeBlocks(row.Message)
		parts := blocksToParts(blocks, dispatchResults)
		if len(parts) == 0 {
			continue
		}
		turns = append(turns, agent.Turn{
			Role:      role,
			Seq:       len(turns),
			Timestamp: strings.TrimSpace(row.Timestamp),
			Parts:     parts,
		})
	}
	return turns, nil
}

func scanRows(f *os.File) ([]jsonlRow, error) {
	var rows []jsonlRow
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row jsonlRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue // skip malformed lines, same as readAllMessages
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

// transcriptBlock is one content block in a message (assistant or tool_result).
type transcriptBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// decodeBlocks returns the message's content as ordered blocks. A bare-string
// content (user text) becomes a single synthetic text block.
func decodeBlocks(raw json.RawMessage) []transcriptBlock {
	if len(raw) == 0 {
		return nil
	}
	var rc rowContent
	if err := json.Unmarshal(raw, &rc); err != nil || len(rc.Content) == 0 {
		return nil
	}
	if rc.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(rc.Content, &s); err == nil && strings.TrimSpace(s) != "" {
			return []transcriptBlock{{Type: "text", Text: s}}
		}
		return nil
	}
	var blocks []transcriptBlock
	if err := json.Unmarshal(rc.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}

// collectDispatchResults indexes dispatch tool_result payloads by the tool_use
// id that produced them. Only mcp__bbclaw__dispatch results are tracked.
func collectDispatchResults(rows []jsonlRow) map[string]*agent.DispatchStatus {
	// First map tool_use.id → name (from assistant rows) so we can filter
	// tool_result frames down to dispatch results only.
	dispatchIDs := map[string]bool{}
	for _, row := range rows {
		if strings.ToLower(strings.TrimSpace(row.Type)) != "assistant" {
			continue
		}
		for _, b := range decodeBlocks(row.Message) {
			if b.Type == "tool_use" && isMCPBBClawDispatch(b.Name) {
				dispatchIDs[b.ID] = true
			}
		}
	}
	results := map[string]*agent.DispatchStatus{}
	for _, row := range rows {
		if strings.ToLower(strings.TrimSpace(row.Type)) != "user" {
			continue
		}
		for _, b := range decodeBlocks(row.Message) {
			if b.Type == "tool_result" && dispatchIDs[b.ToolUseID] {
				results[b.ToolUseID] = parseDispatchResult(b.ToolUseID, b.Content)
			}
		}
	}
	return results
}

// blocksToParts converts one message's content blocks into ordered Parts.
// Dispatch blocks are enriched with their matched tool_result status.
func blocksToParts(blocks []transcriptBlock, dispatchResults map[string]*agent.DispatchStatus) []agent.Part {
	var parts []agent.Part
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, agent.Part{Kind: "text", Text: capBytes(t, maxContentBytes)})
			}
		case "thinking":
			if t := strings.TrimSpace(b.Thinking); t != "" {
				parts = append(parts, agent.Part{Kind: "thinking", Text: capBytes(t, maxThinkingBytes)})
			}
		case "tool_use":
			if isMCPBBClawDispatch(b.Name) {
				cwd, title := parseDispatchInput(b.Input)
				dp := &agent.DispatchPart{TaskID: b.ID, Cwd: cwd, Title: title, Status: "started"}
				if res, ok := dispatchResults[b.ID]; ok && res != nil {
					if res.Phase != "" {
						dp.Status = res.Phase
					}
					if res.TaskID != "" {
						dp.TaskID = res.TaskID
					}
					dp.ElapsedMs = res.ElapsedMs
					dp.Error = res.ErrorMsg
					dp.ChildSessionID = res.ChildSessionID
				}
				parts = append(parts, agent.Part{Kind: "dispatch", Dispatch: dp})
			} else if name := strings.TrimSpace(b.Name); name != "" {
				parts = append(parts, agent.Part{Kind: "tool", Tool: name, Text: summarizeToolInput(b.Name, b.Input)})
			}
			// tool_result blocks are intentionally dropped here: dispatch results
			// are folded into the dispatch part above; other tool_results are not
			// shown (ADR-029 §2.1).
		}
	}
	return parts
}

// capBytes truncates s to <= maxBytes on a rune boundary, appending an ellipsis
// when it had to cut. Reuses safeTruncateBytes (load_messages.go).
func capBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return safeTruncateBytes(s, maxBytes) + "…"
}
