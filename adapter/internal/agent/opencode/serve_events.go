package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	opencode "github.com/sst/opencode-sdk-go"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// serveEvent is a version-skew-proof decode of any `/event` payload. We read
// only the `properties` we map, ignoring everything else — the same discipline
// the spike proved (ADR-031): the server emits richer/newer events than the SDK
// models, so we key off the type string and parse raw rather than the SDK union.
type serveEvent struct {
	Properties struct {
		SessionID string `json:"sessionID"`
		Field     string `json:"field"` // message.part.delta: "text" | "reasoning" | ...
		Delta     string `json:"delta"`
		ID        string `json:"id"`
		Part      struct {
			Type   string `json:"type"` // text | reasoning | tool | step-finish | subtask | agent
			Tool   string `json:"tool"`
			Text   string `json:"text"`
			CallID string `json:"callID"`
			State  struct {
				Status string          `json:"status"`
				Title  string          `json:"title"`
				Input  json.RawMessage `json:"input"`
			} `json:"state"`
			Tokens *struct {
				Input  int `json:"input"`
				Output int `json:"output"`
			} `json:"tokens"`
		} `json:"part"`
		Permission struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		} `json:"permission"`
	} `json:"properties"`
}

// runEventRouter consumes the server's GLOBAL SSE event stream and fans each
// event out to the owning session's channel. It runs for the driver lifetime,
// reconnecting if the stream drops (e.g. serve restart).
//
// We read GET /global/event with a raw SSE reader rather than the SDK's
// Event.ListStreaming, for two reasons (ADR-031):
//   - /event is scoped to the server's startup project; our sessions run in
//     arbitrary project directories, so only /global/event sees their events.
//   - parsing raw `data:` JSON decouples us entirely from the SDK's event-union
//     version (the installed server emits events the SDK build doesn't model).
func (d *ServeDriver) runEventRouter(ctx context.Context, _ *opencode.Client) {
	for ctx.Err() == nil {
		if err := d.streamGlobalEvents(ctx); err != nil && ctx.Err() == nil {
			d.log.Warnf("opencode serve: event stream error: %v; reconnecting", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (d *ServeDriver) streamGlobalEvents(ctx context.Context) error {
	d.mu.Lock()
	base := d.baseURL
	d.mu.Unlock()
	if base == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/global/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("/global/event status %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// SSE: we only care about `data:` lines; each carries one full JSON event.
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		// /global/event wraps each event: {directory, project, payload:{type,
		// properties}}. Unwrap to the inner payload (the same shape /event emits
		// unwrapped). "sync" payloads are redundant snapshots — skip them.
		var env struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(data), &env); err != nil || len(env.Payload) == 0 {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(env.Payload, &head); err != nil || head.Type == "" || head.Type == "sync" {
			continue
		}
		d.dispatchEvent(ctx, head.Type, string(env.Payload))
	}
	return sc.Err()
}

func (d *ServeDriver) dispatchEvent(ctx context.Context, typ, raw string) {
	var e serveEvent
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return
	}
	pr := e.Properties
	sid := pr.SessionID
	if sid == "" {
		sid = pr.Permission.SessionID
	}

	switch typ {
	case "message.part.delta":
		s := d.sessionByOC(sid)
		if s == nil || pr.Delta == "" {
			return
		}
		switch pr.Field {
		case "text":
			s.emit(agent.Event{Type: agent.EvText, Text: pr.Delta})
		case "reasoning", "thinking":
			s.emit(agent.Event{Type: agent.EvThinking, Text: pr.Delta})
		}

	case "message.part.updated":
		s := d.sessionByOC(sid)
		if s == nil {
			return
		}
		switch pr.Part.Type {
		case "tool":
			// Display-only step. Only surface the first observation (pending/
			// running) to avoid one EvToolCall per state transition.
			if pr.Part.State.Status == "" || pr.Part.State.Status == "pending" || pr.Part.State.Status == "running" {
				s.emit(agent.Event{Type: agent.EvToolCall, Tool: &agent.ToolCall{
					ID:   agent.ToolID(pr.Part.CallID),
					Tool: pr.Part.Tool,
					Hint: toolHint(pr.Part.State.Title, pr.Part.State.Input),
				}})
			}
		case "step-finish", "step_finish":
			if pr.Part.Tokens != nil {
				s.emit(agent.Event{Type: agent.EvTokens, Tokens: &agent.Tokens{
					In:  pr.Part.Tokens.Input,
					Out: pr.Part.Tokens.Output,
				}})
			}
		case "subtask", "agent":
			// Butler sub-task dispatch (ADR-024/029). v1: surface as a coarse
			// dispatch status; richer mapping (child session drill-in) is a
			// fast-follow once the butler MCP path lands.
			s.emit(agent.Event{Type: agent.EvDispatchStatus, Dispatch: &agent.DispatchStatus{
				Phase: "running",
				Title: pr.Part.Tool,
			}})
		}

	case "permission.asked", "permission.updated":
		// v1: ToolApproval=false → auto-approve so a permission-gated serve does
		// not hang. (Device-side approval UX is a fast-follow.)
		permID := firstNonEmptyStr(pr.ID, pr.Permission.ID)
		if permID != "" && sid != "" {
			d.mu.Lock()
			client := d.client
			d.mu.Unlock()
			if client != nil {
				_, err := client.Session.Permissions.Respond(ctx, sid, permID, opencode.SessionPermissionRespondParams{
					Response: opencode.F(opencode.SessionPermissionRespondParamsResponseOnce),
				})
				if err != nil {
					d.log.Warnf("opencode serve: auto-approve permission %s failed: %v", permID, err)
				}
			}
		}

	case "session.error":
		if s := d.sessionByOC(sid); s != nil {
			s.emit(agent.Event{Type: agent.EvError, Text: truncate(raw, 400)})
		}
	}
}

func (d *ServeDriver) sessionByOC(ocID string) *serveSession {
	if ocID == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.byOC[ocID]
}

// toolHint renders a short, display-suitable hint from a tool's state.
func toolHint(title string, input json.RawMessage) string {
	if title != "" {
		return truncate(title, 80)
	}
	if len(input) == 0 {
		return ""
	}
	var f struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Pattern  string `json:"pattern"`
	}
	if err := json.Unmarshal(input, &f); err != nil {
		return ""
	}
	switch {
	case f.Command != "":
		return truncate(f.Command, 80)
	case f.FilePath != "":
		return truncate(f.FilePath, 80)
	case f.Pattern != "":
		return truncate(f.Pattern, 80)
	}
	return ""
}

func firstNonEmptyStr(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
