package opencode

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// MessageLoader + PartLoader for the serve driver (ADR-013 / ADR-029 history
// replay). Both read GET /session/:id/message — a stable JSON shape — and
// paginate in memory. before is the upper-exclusive seq cursor; before <= 0
// means "the latest page".

// ocMessage mirrors one /session/:id/message entry.
type ocMessage struct {
	Info struct {
		Role string `json:"role"`
		Time struct {
			Created   int64 `json:"created"`
			Completed int64 `json:"completed"`
		} `json:"time"`
	} `json:"info"`
	Parts []ocMsgPart `json:"parts"`
}

type ocMsgPart struct {
	Type  string `json:"type"` // text | reasoning | tool | step-start | step-finish | subtask | agent
	Text  string `json:"text"`
	Tool  string `json:"tool"`
	State struct {
		Title  string          `json:"title"`
		Input  json.RawMessage `json:"input"`
		Output string          `json:"output"`
	} `json:"state"`
}

func (d *ServeDriver) loadRawMessages(ctx context.Context, sid string) ([]ocMessage, error) {
	if err := d.ensureReady(); err != nil {
		return nil, err
	}
	var msgs []ocMessage
	if err := d.getJSON(ctx, "/session/"+sid+"/message", &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// LoadMessages implements agent.MessageLoader — flat text transcript.
func (d *ServeDriver) LoadMessages(ctx context.Context, sid string, before, limit int) (agent.MessagesPage, error) {
	msgs, err := d.loadRawMessages(ctx, sid)
	if err != nil {
		return agent.MessagesPage{}, err
	}
	total := len(msgs)
	lo, hi := pageBounds(total, before, limit)
	out := make([]agent.Message, 0, hi-lo)
	for i := lo; i < hi; i++ {
		m := msgs[i]
		out = append(out, agent.Message{
			Role:      m.Info.Role,
			Content:   flattenText(m.Parts),
			Seq:       i,
			Timestamp: rfc3339Millis(m.Info.Time.Created),
		})
	}
	return agent.MessagesPage{Messages: out, Total: total, HasMore: lo > 0}, nil
}

// LoadParts implements agent.PartLoader — structured thinking/text/tool/dispatch.
func (d *ServeDriver) LoadParts(ctx context.Context, sid string, before, limit int) (agent.PartsPage, error) {
	msgs, err := d.loadRawMessages(ctx, sid)
	if err != nil {
		return agent.PartsPage{}, err
	}
	total := len(msgs)
	lo, hi := pageBounds(total, before, limit)
	turns := make([]agent.Turn, 0, hi-lo)
	for i := lo; i < hi; i++ {
		m := msgs[i]
		turns = append(turns, agent.Turn{
			Role:      m.Info.Role,
			Seq:       i,
			Timestamp: rfc3339Millis(m.Info.Time.Created),
			Parts:     d.mapParts(m.Parts),
		})
	}
	return agent.PartsPage{Turns: turns, Total: total, HasMore: lo > 0}, nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

// pageBounds returns the [lo,hi) slice window for a transcript of n messages.
// before<=0 → last `limit`; else messages with seq < before, capped at `limit`.
func pageBounds(n, before, limit int) (lo, hi int) {
	if limit <= 0 {
		limit = 50
	}
	hi = n
	if before > 0 && before < n {
		hi = before
	}
	lo = hi - limit
	if lo < 0 {
		lo = 0
	}
	return lo, hi
}

func flattenText(parts []ocMsgPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func (d *ServeDriver) mapParts(parts []ocMsgPart) []agent.Part {
	out := make([]agent.Part, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, agent.Part{Kind: "text", Text: p.Text})
			}
		case "reasoning":
			if p.Text != "" {
				out = append(out, agent.Part{Kind: "thinking", Text: p.Text})
			}
		case "tool":
			if d.isDispatchTool(p.Tool) {
				// Butler dispatch call woven inline (ADR-029 §2.4). Reconstruct the
				// full DispatchPart from the tool's input (cwd/title) and output
				// (status/elapsed/childSessionId) so the admin page can drill into
				// the worker's transcript (ADR-029 §2.3, P2-6).
				cwd, title := parseDispatchInput(p.State.Input)
				ds := parseDispatchResult("", p.State.Output)
				out = append(out, agent.Part{Kind: "dispatch", Dispatch: &agent.DispatchPart{
					TaskID:         ds.TaskID,
					Cwd:            cwd,
					Title:          firstNonEmptyStr(title, p.State.Title),
					Status:         ds.Phase,
					ElapsedMs:      ds.ElapsedMs,
					Error:          ds.ErrorMsg,
					ChildSessionID: ds.ChildSessionID,
				}})
			} else {
				out = append(out, agent.Part{Kind: "tool", Tool: p.Tool, Text: p.State.Title})
			}
		case "subtask", "agent":
			out = append(out, agent.Part{Kind: "dispatch", Dispatch: &agent.DispatchPart{Title: p.Tool}})
		}
		// step-start / step-finish / snapshot / patch are not rendered.
	}
	return out
}

func rfc3339Millis(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

var (
	_ agent.MessageLoader = (*ServeDriver)(nil)
	_ agent.PartLoader    = (*ServeDriver)(nil)
)
