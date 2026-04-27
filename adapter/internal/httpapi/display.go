package httpapi

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type displayTask struct {
	TaskID      string              `json:"taskId"`
	DeviceID    string              `json:"deviceId"`
	Title       string              `json:"title,omitempty"`
	Body        string              `json:"body,omitempty"`
	Priority    string              `json:"priority,omitempty"`
	Category    string              `json:"category,omitempty"`
	DisplayText string              `json:"displayText,omitempty"`
	CreatedAtMs int64               `json:"createdAtMs"`
	Blocks      []displayTaskBlock  `json:"blocks,omitempty"`
	Actions     []displayTaskAction `json:"actions,omitempty"`
}

type displayTaskBlock struct {
	Type  string `json:"type"`
	Label string `json:"label,omitempty"`
	Text  string `json:"text,omitempty"`
	Value string `json:"value,omitempty"`
	Tone  string `json:"tone,omitempty"`
}

type displayTaskAction struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type displayAck struct {
	DeviceID string `json:"deviceId"`
	TaskID   string `json:"taskId"`
	ActionID string `json:"actionId,omitempty"`
}

type displayTaskQueue struct {
	mu       sync.Mutex
	maxDepth int
	byDevice map[string][]displayTask
}

func newDisplayTaskQueue(maxDepth int) *displayTaskQueue {
	if maxDepth <= 0 {
		maxDepth = 64
	}
	return &displayTaskQueue{
		maxDepth: maxDepth,
		byDevice: make(map[string][]displayTask),
	}
}

func (q *displayTaskQueue) enqueue(task displayTask) displayTask {
	q.mu.Lock()
	defer q.mu.Unlock()

	t := normalizeDisplayTask(task)
	list := append(q.byDevice[t.DeviceID], t)
	if len(list) > q.maxDepth {
		list = list[len(list)-q.maxDepth:]
	}
	q.byDevice[t.DeviceID] = list
	return t
}

func (q *displayTaskQueue) pull(deviceID string) (displayTask, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	list := q.byDevice[deviceID]
	if len(list) == 0 {
		return displayTask{}, false
	}
	task := list[0]
	rest := list[1:]
	if len(rest) == 0 {
		delete(q.byDevice, deviceID)
	} else {
		q.byDevice[deviceID] = rest
	}
	return task, true
}

func normalizeDisplayTask(task displayTask) displayTask {
	task.DeviceID = strings.TrimSpace(task.DeviceID)
	task.TaskID = strings.TrimSpace(task.TaskID)
	if task.TaskID == "" {
		task.TaskID = fmt.Sprintf("task-%d", time.Now().UnixMilli())
	}
	task.Priority = strings.TrimSpace(task.Priority)
	if task.Priority == "" {
		task.Priority = "normal"
	}
	task.CreatedAtMs = time.Now().UnixMilli()
	task.DisplayText = strings.TrimSpace(task.DisplayText)
	if task.DisplayText == "" {
		task.DisplayText = buildDisplayText(task)
	}
	return task
}

func buildDisplayText(task displayTask) string {
	parts := []string{}
	if title := strings.TrimSpace(task.Title); title != "" {
		parts = append(parts, title)
	}
	if body := strings.TrimSpace(task.Body); body != "" {
		parts = append(parts, body)
	}
	for _, block := range task.Blocks {
		label := strings.TrimSpace(block.Label)
		text := strings.TrimSpace(block.Text)
		value := strings.TrimSpace(block.Value)
		switch {
		case label != "" && value != "":
			parts = append(parts, label+": "+value)
		case label != "" && text != "":
			parts = append(parts, label+": "+text)
		case text != "":
			parts = append(parts, text)
		case value != "":
			parts = append(parts, value)
		}
	}
	msg := strings.Join(parts, " | ")
	if len(msg) > 280 {
		return msg[:280]
	}
	return msg
}
