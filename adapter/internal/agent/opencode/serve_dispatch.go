package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/daboluocc/bbclaw/adapter/internal/agent"
)

// Butler dispatch for the serve driver (ADR-021-firmware-ui §1.2, ADR-024 §5).
// The conversational butler dispatches coding work to worker agents through an
// MCP server the adapter provides (butlermcp, server key "bbclaw"). We register
// that server on the shared `opencode serve` via POST /mcp, then translate the
// model's calls to the `<server>_dispatch` tool into EvDispatchStatus events the
// butler.Engine ring buffer + device status line consume — mirroring the
// claudecode driver's mcp__bbclaw__dispatch handling.

// registerMCPServers registers each spec on the shared serve exactly once
// (POST /mcp). It records the implied dispatch tool name (`<name>_dispatch`)
// so the router can distinguish dispatch calls from ordinary tool steps.
func (d *ServeDriver) registerMCPServers(ctx context.Context, specs []agent.MCPServerSpec) error {
	if err := d.ensureReady(); err != nil {
		return err
	}
	var firstErr error
	for _, spec := range specs {
		if spec.Name == "" || spec.Command == "" {
			continue
		}
		d.mu.Lock()
		already := d.registeredMCP[spec.Name]
		d.mu.Unlock()
		if already {
			continue
		}

		body := map[string]any{
			"name": spec.Name,
			"config": map[string]any{
				"type":        "local",
				"command":     append([]string{spec.Command}, spec.Args...),
				"environment": spec.Env,
				"enabled":     true,
			},
		}
		if err := d.postJSON(ctx, "/mcp", body); err != nil {
			d.log.Warnf("opencode serve: POST /mcp %q failed: %v", spec.Name, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		d.mu.Lock()
		d.registeredMCP[spec.Name] = true
		d.dispatchTools[spec.Name+"_dispatch"] = true
		d.mu.Unlock()
		d.log.Infof("opencode serve: registered MCP server %q (dispatch tool %q)", spec.Name, spec.Name+"_dispatch")
	}
	return firstErr
}

// isDispatchTool reports whether a tool name is a butler dispatch tool. It
// checks the exact registered names first, then a lenient fallback (a
// registered server's prefix + "dispatch") to tolerate opencode tool-naming
// variations (e.g. "bbclaw_dispatch" vs "bbclaw*dispatch").
func (d *ServeDriver) isDispatchTool(name string) bool {
	if name == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dispatchTools[name] {
		return true
	}
	lower := strings.ToLower(name)
	if !strings.Contains(lower, "dispatch") {
		return false
	}
	for server := range d.registeredMCP {
		if strings.HasPrefix(name, server) {
			return true
		}
	}
	return false
}

func (d *ServeDriver) postJSON(ctx context.Context, path string, body any) error {
	d.mu.Lock()
	base := d.baseURL
	d.mu.Unlock()
	if base == "" {
		return fmt.Errorf("opencode serve: not started")
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("opencode serve: POST %s → %d", path, resp.StatusCode)
	}
	return nil
}

// emitDispatch maps a dispatch tool's state transitions to EvDispatchStatus:
// the first pending/running observation → "started" (with cwd/title from the
// input), "completed" → the terminal status parsed from the output (done/async/
// error + elapsed + childSessionID), "error" → an error phase.
func (d *ServeDriver) emitDispatch(s *serveSession, callID, status string, input json.RawMessage, output, errMsg string) {
	switch {
	case isToolActive(status):
		if s.markDispStarted(callID) {
			cwd, title := parseDispatchInput(input)
			s.emit(agent.Event{Type: agent.EvDispatchStatus, Dispatch: &agent.DispatchStatus{
				Phase: "started", TaskID: callID, Cwd: cwd, Title: title,
			}})
		}
	case status == "completed":
		s.emit(agent.Event{Type: agent.EvDispatchStatus, Dispatch: parseDispatchResult(callID, output)})
	case status == "error":
		s.emit(agent.Event{Type: agent.EvDispatchStatus, Dispatch: &agent.DispatchStatus{
			Phase: "error", TaskID: callID, ErrorMsg: errMsg,
		}})
	}
}

// isToolActive reports whether a tool state.status is a pre-completion state.
func isToolActive(status string) bool {
	return status == "" || status == "pending" || status == "running"
}

// ── dispatch payload parsing (mirrors claudecode) ───────────────────────────

// parseDispatchInput extracts cwd + a short title from the dispatch tool's
// input JSON ({cwd, prompt}; legacy {project, task}).
func parseDispatchInput(raw json.RawMessage) (cwd, title string) {
	if len(raw) == 0 {
		return "", ""
	}
	var in struct {
		Cwd     string `json:"cwd"`
		Prompt  string `json:"prompt"`
		Project string `json:"project"`
		Task    string `json:"task"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", ""
	}
	cwd = strings.TrimSpace(in.Cwd)
	if cwd == "" {
		cwd = strings.TrimSpace(in.Project)
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(in.Task)
	}
	return cwd, truncateCJK(prompt, 24)
}

// dispatchResult is the JSON the dispatch MCP tool returns in its output.
type dispatchResult struct {
	Status         string `json:"status"` // done | running | async | error
	TaskID         string `json:"taskId"`
	ElapsedMs      int64  `json:"elapsedMs"`
	Error          string `json:"error"`
	ChildSessionID string `json:"childSessionId"`
}

// parseDispatchResult parses the dispatch tool's output string into a terminal
// DispatchStatus. callID is the TaskID fallback. Output may be a bare JSON
// object or a JSON string wrapping one.
func parseDispatchResult(callID, output string) *agent.DispatchStatus {
	ds := &agent.DispatchStatus{TaskID: callID, Phase: "done"}
	text := strings.TrimSpace(output)
	if text == "" {
		return ds
	}
	// Output may itself be a JSON-encoded string.
	if len(text) > 0 && text[0] == '"' {
		var unq string
		if json.Unmarshal([]byte(text), &unq) == nil {
			text = unq
		}
	}
	var res dispatchResult
	if json.Unmarshal([]byte(text), &res) == nil {
		if res.TaskID != "" {
			ds.TaskID = res.TaskID
		}
		if res.Status != "" {
			phase := res.Status
			if phase == "running" {
				phase = "async"
			}
			ds.Phase = phase
		}
		ds.ElapsedMs = res.ElapsedMs
		ds.ErrorMsg = res.Error
		ds.ChildSessionID = res.ChildSessionID
	}
	return ds
}

func truncateCJK(s string, maxCJK int) string {
	runes := []rune(s)
	if len(runes) <= maxCJK {
		return s
	}
	return string(runes[:maxCJK]) + "…"
}
