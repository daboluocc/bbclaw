package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/daboluocc/bbclaw/adapter/internal/obs"
	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type DeviceConn struct {
	conn     *websocket.Conn
	deviceID string
	mu       sync.Mutex
	closed   bool
}

func (dc *DeviceConn) WriteJSON(v any) error {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if dc.closed {
		return websocket.ErrCloseSent
	}
	return dc.conn.WriteJSON(v)
}

func (dc *DeviceConn) Close() {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if !dc.closed {
		dc.closed = true
		dc.conn.Close()
	}
}

type WSHub struct {
	mu      sync.Mutex
	devices map[string]*DeviceConn
	log     *obs.Logger
}

func newWSHub(log *obs.Logger) *WSHub {
	return &WSHub{
		devices: make(map[string]*DeviceConn),
		log:     log,
	}
}

func (h *WSHub) Register(deviceID string, conn *websocket.Conn) *DeviceConn {
	dc := &DeviceConn{conn: conn, deviceID: deviceID}
	h.mu.Lock()
	if old, ok := h.devices[deviceID]; ok {
		old.Close()
	}
	h.devices[deviceID] = dc
	h.mu.Unlock()
	h.log.Infof("ws: device registered id=%s", deviceID)
	return dc
}

func (h *WSHub) Unregister(deviceID string) {
	h.mu.Lock()
	delete(h.devices, deviceID)
	h.mu.Unlock()
	h.log.Infof("ws: device unregistered id=%s", deviceID)
}

func (h *WSHub) Broadcast(payload any) bool {
	h.mu.Lock()
	conns := make([]*DeviceConn, 0, len(h.devices))
	for _, dc := range h.devices {
		conns = append(conns, dc)
	}
	h.mu.Unlock()

	if len(conns) == 0 {
		return false
	}
	for _, dc := range conns {
		if err := dc.WriteJSON(payload); err != nil {
			h.log.Warnf("ws: broadcast write failed device=%s err=%v", dc.deviceID, err)
		}
	}
	return true
}

func (h *WSHub) Send(deviceID string, payload any) bool {
	h.mu.Lock()
	dc, ok := h.devices[deviceID]
	h.mu.Unlock()
	if !ok {
		return false
	}
	if err := dc.WriteJSON(payload); err != nil {
		h.log.Warnf("ws: send failed device=%s err=%v", deviceID, err)
		return false
	}
	return true
}

type wsEnvelope struct {
	Type    string         `json:"type"`
	Kind    string         `json:"kind,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.wsHub == nil {
		http.Error(w, "WebSocket not available", http.StatusServiceUnavailable)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Errorf("ws: upgrade failed err=%v", err)
		return
	}

	deviceID := strings.TrimSpace(r.URL.Query().Get("deviceId"))
	if deviceID == "" {
		deviceID = "local"
	}

	dc := s.wsHub.Register(deviceID, conn)
	defer func() {
		s.wsHub.Unregister(deviceID)
		dc.Close()
	}()

	if s.notifQueue != nil {
		s.notifQueue.FlushTo(deviceID, dc)
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				s.log.Infof("ws: read closed device=%s err=%v", deviceID, err)
			}
			return
		}
		s.handleWSMessage(deviceID, msg)
	}
}

func (s *Server) handleWSMessage(deviceID string, msg []byte) {
	var env wsEnvelope
	if err := json.Unmarshal(msg, &env); err != nil {
		s.log.Warnf("ws: invalid message from device=%s err=%v", deviceID, err)
		return
	}

	switch strings.ToLower(strings.TrimSpace(env.Kind)) {
	case "session.notification.ack":
		if s.notifQueue != nil {
			sid, _ := env.Payload["sessionId"].(string)
			if sid != "" {
				s.notifQueue.AckSession(sid)
				s.log.Infof("ws: notification ack device=%s session=%s", deviceID, sid)
			}
		}
	default:
		s.log.Infof("ws: unhandled message device=%s kind=%s", deviceID, env.Kind)
	}
}
