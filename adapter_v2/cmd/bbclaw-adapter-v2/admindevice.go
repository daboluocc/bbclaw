package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/curdevice"
)

// deviceConfigSetter pushes a partial volume/miyu config to a device. It is a var
// so tests can stub the cloud round-trip; in production it is postDeviceConfig —
// the SAME cloud-config path the `device set-volume/set-miyu` CLI uses (device.go).
var deviceConfigSetter = func(deviceID string, body setConfigRequest) (*setConfigResponse, error) {
	return postDeviceConfig(deviceID, body)
}

// adminWriteJSON writes the {ok,data,error,detail} envelope the admin page expects
// (adminapi uses an unexported writer; this mirrors it for the package-main handler).
func adminWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// adminDeviceConfigHandler is the loopback admin endpoint /v1/admin/device-config.
// It lets the admin page control the CURRENT connected device's 密语(miyu) and
// volume the SAME way the butler does over voice — through the cloud config path
// (postDeviceConfig → POST /v1/devices/{id}/config → cloud pushes config.update over
// WS → firmware). The target device is curdevice.Get() (the last device the adapter
// saw), so the page never needs a device id. Cloud_saas only: with no paired cloud
// or no connected device the call returns ok=false + a friendly reason.
//
//	GET  /v1/admin/device-config → {ok,data:{deviceId, cloudReady}}
//	POST /v1/admin/device-config   body {volumePct?:int, miyuEnabled?:bool}
//	     → {ok,data:{deviceId, version, volumePct, miyuEnabled}}
func adminDeviceConfigHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(curdevice.Get())
		cloudReady := strings.TrimSpace(os.Getenv("CLOUD_AUTH_TOKEN")) != ""

		switch r.Method {
		case http.MethodGet:
			adminWriteJSON(w, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{
				"deviceId": id, "cloudReady": cloudReady,
			}})
		case http.MethodPost:
			var body struct {
				VolumePct   *int  `json:"volumePct"`
				MiyuEnabled *bool `json:"miyuEnabled"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
				adminWriteJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "INVALID_REQUEST", "detail": err.Error()})
				return
			}
			if body.VolumePct == nil && body.MiyuEnabled == nil {
				adminWriteJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "INVALID_REQUEST", "detail": "需提供 volumePct 或 miyuEnabled"})
				return
			}
			if id == "" {
				// ok=false at 200 so the page shows a friendly reason (like pick-dir).
				adminWriteJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "NO_DEVICE",
					"detail": "当前没有连接的设备（需 cloud_saas 配对的设备先连上）"})
				return
			}
			req := setConfigRequest{}
			if body.VolumePct != nil {
				v := *body.VolumePct
				if v < 0 {
					v = 0
				}
				if v > 100 {
					v = 100
				}
				req.VolumePct = &v
			}
			if body.MiyuEnabled != nil {
				req.MiyuEnabled = body.MiyuEnabled
			}
			res, err := deviceConfigSetter(id, req)
			if err != nil {
				adminWriteJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "CLOUD_ERROR", "detail": err.Error()})
				return
			}
			adminWriteJSON(w, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{
				"deviceId": id, "version": res.Data.Version,
				"volumePct": res.Data.VolumePct, "miyuEnabled": res.Data.MiyuEnabled,
			}})
		default:
			w.Header().Set("Allow", "GET, POST")
			adminWriteJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "METHOD_NOT_ALLOWED"})
		}
	}
}
