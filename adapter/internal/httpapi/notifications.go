package httpapi

import (
	"sync"
	"time"
)

type SessionNotification struct {
	SessionID string `json:"sessionId"`
	Driver    string `json:"driver"`
	Type      string `json:"type"`
	Preview   string `json:"preview"`
	Timestamp int64  `json:"timestamp"`
}

type NotificationQueue struct {
	mu     sync.Mutex
	events map[string][]SessionNotification
	maxLen int
}

func newNotificationQueue(maxLen int) *NotificationQueue {
	return &NotificationQueue{
		events: make(map[string][]SessionNotification),
		maxLen: maxLen,
	}
}

func (q *NotificationQueue) Enqueue(deviceID string, n SessionNotification) {
	q.mu.Lock()
	defer q.mu.Unlock()
	list := q.events[deviceID]
	if len(list) >= q.maxLen {
		list = list[1:]
	}
	q.events[deviceID] = append(list, n)
}

func (q *NotificationQueue) FlushTo(deviceID string, dc *DeviceConn) {
	q.mu.Lock()
	pending := q.events[deviceID]
	delete(q.events, deviceID)
	q.mu.Unlock()

	for _, n := range pending {
		env := map[string]any{
			"type": "event",
			"kind": "session.notification",
			"payload": map[string]any{
				"sessionId": n.SessionID,
				"driver":    n.Driver,
				"type":      n.Type,
				"preview":   n.Preview,
				"timestamp": n.Timestamp,
			},
		}
		_ = dc.WriteJSON(env)
	}
}

func (q *NotificationQueue) AckSession(sessionID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for deviceID, list := range q.events {
		filtered := list[:0]
		for _, n := range list {
			if n.SessionID != sessionID {
				filtered = append(filtered, n)
			}
		}
		if len(filtered) == 0 {
			delete(q.events, deviceID)
		} else {
			q.events[deviceID] = filtered
		}
	}
}

func truncatePreview(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

func (s *Server) pushNotification(notif SessionNotification) {
	if notif.Timestamp == 0 {
		notif.Timestamp = time.Now().UnixMilli()
	}
	notif.Preview = truncatePreview(notif.Preview, 48)

	env := map[string]any{
		"type": "event",
		"kind": "session.notification",
		"payload": map[string]any{
			"sessionId": notif.SessionID,
			"driver":    notif.Driver,
			"type":      notif.Type,
			"preview":   notif.Preview,
			"timestamp": notif.Timestamp,
		},
	}

	pushed := false
	if s.wsHub != nil {
		pushed = s.wsHub.Broadcast(env)
	}

	if !pushed && s.notifQueue != nil {
		s.notifQueue.Enqueue("local", notif)
	}

	s.log.Infof("notification: pushed=%v session=%s driver=%s type=%s preview=%q",
		pushed, notif.SessionID, notif.Driver, notif.Type, notif.Preview)
}
