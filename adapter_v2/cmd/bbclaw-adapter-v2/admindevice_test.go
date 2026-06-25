package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daboluocc/bbclaw/adapter_v2/internal/curdevice"
)

// withDevice points curdevice at a temp data dir and records id as "current".
func withDevice(t *testing.T, id string) {
	t.Helper()
	t.Setenv("BBCLAW_ADAPTER_V2_DATA_DIR", t.TempDir())
	if id != "" {
		if err := curdevice.Record(id); err != nil {
			t.Fatalf("record device: %v", err)
		}
	}
}

func TestAdminDeviceConfigGet(t *testing.T) {
	withDevice(t, "dev-7")
	rr := httptest.NewRecorder()
	adminDeviceConfigHandler()(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/device-config", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"deviceId":"dev-7"`) {
		t.Errorf("GET should report current device: %s", rr.Body.String())
	}
}

func TestAdminDeviceConfigPostMiyu(t *testing.T) {
	withDevice(t, "dev-7")
	orig := deviceConfigSetter
	defer func() { deviceConfigSetter = orig }()
	var gotID string
	var gotReq setConfigRequest
	deviceConfigSetter = func(id string, body setConfigRequest) (*setConfigResponse, error) {
		gotID, gotReq = id, body
		res := &setConfigResponse{OK: true}
		res.Data.Version = 5
		res.Data.MiyuEnabled = true
		res.Data.VolumePct = 40
		return res, nil
	}
	rr := httptest.NewRecorder()
	adminDeviceConfigHandler()(rr, httptest.NewRequest(http.MethodPost, "/v1/admin/device-config",
		strings.NewReader(`{"miyuEnabled":true}`)))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"miyuEnabled":true`) {
		t.Fatalf("post miyu failed: %d %s", rr.Code, rr.Body.String())
	}
	if gotID != "dev-7" {
		t.Errorf("setter got device %q, want dev-7", gotID)
	}
	if gotReq.MiyuEnabled == nil || !*gotReq.MiyuEnabled || gotReq.VolumePct != nil {
		t.Errorf("setter got req %+v, want only miyuEnabled=true", gotReq)
	}
}

func TestAdminDeviceConfigClampsVolume(t *testing.T) {
	withDevice(t, "dev-7")
	orig := deviceConfigSetter
	defer func() { deviceConfigSetter = orig }()
	var gotReq setConfigRequest
	deviceConfigSetter = func(id string, body setConfigRequest) (*setConfigResponse, error) {
		gotReq = body
		return &setConfigResponse{OK: true}, nil
	}
	rr := httptest.NewRecorder()
	adminDeviceConfigHandler()(rr, httptest.NewRequest(http.MethodPost, "/v1/admin/device-config",
		strings.NewReader(`{"volumePct":250}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if gotReq.VolumePct == nil || *gotReq.VolumePct != 100 {
		t.Errorf("volume not clamped to 100: %+v", gotReq)
	}
}

func TestAdminDeviceConfigNoDevice(t *testing.T) {
	withDevice(t, "") // no current device recorded
	rr := httptest.NewRecorder()
	adminDeviceConfigHandler()(rr, httptest.NewRequest(http.MethodPost, "/v1/admin/device-config",
		strings.NewReader(`{"miyuEnabled":true}`)))
	b := rr.Body.String()
	if !strings.Contains(b, `"ok":false`) || !strings.Contains(b, "NO_DEVICE") {
		t.Errorf("no-device should be ok=false NO_DEVICE: %s", b)
	}
}

func TestAdminDeviceConfigEmptyBody(t *testing.T) {
	withDevice(t, "dev-7")
	rr := httptest.NewRecorder()
	adminDeviceConfigHandler()(rr, httptest.NewRequest(http.MethodPost, "/v1/admin/device-config",
		strings.NewReader(`{}`)))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty body should be 400, got %d", rr.Code)
	}
}

func TestAdminDeviceConfigMethod(t *testing.T) {
	withDevice(t, "dev-7")
	rr := httptest.NewRecorder()
	adminDeviceConfigHandler()(rr, httptest.NewRequest(http.MethodPut, "/v1/admin/device-config", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT should be 405, got %d", rr.Code)
	}
}
