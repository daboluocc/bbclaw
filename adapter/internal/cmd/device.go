package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/daboluocc/bbclaw/adapter/internal/homeadapter"
	"github.com/spf13/cobra"
)

// NewDeviceCmd creates the "device" subcommand tree.
func NewDeviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "device",
		Short: "Control BBClaw device settings via cloud API",
	}
	cmd.AddCommand(newSetVolumeCmd())
	cmd.AddCommand(newSetMiyuCmd())
	return cmd
}

func newSetVolumeCmd() *cobra.Command {
	var deviceID string

	cmd := &cobra.Command{
		Use:   "set-volume <pct>",
		Short: "Set device volume (0-100) via cloud config API",
		Long: `Set the volume percentage for a BBClaw device.

The command reads CLOUD_WS_URL and CLOUD_AUTH_TOKEN from the environment
(or .env file) and calls POST /v1/devices/{deviceId}/config on the cloud.

Examples:
  bbclaw-adapter device set-volume 50 --device <deviceId>
  bbclaw-adapter device set-volume 0  --device <deviceId>   # mute
  bbclaw-adapter device set-volume 100 --device <deviceId>  # max`,
		Args: cobra.ExactArgs(1),
		RunE: runSetVolume(&deviceID),
	}

	cmd.Flags().StringVar(&deviceID, "device", "", "Target device ID (required)")
	_ = cmd.MarkFlagRequired("device")

	return cmd
}

// setConfigRequest is the body for POST /v1/devices/{id}/config. Fields are
// pointers + omitempty so a command only ever sends the keys it intends to
// change — otherwise set-miyu would emit volumePct:0 and the cloud would treat
// it as a real value and mute the device.
type setConfigRequest struct {
	VolumePct   *int  `json:"volumePct,omitempty"`
	MiyuEnabled *bool `json:"miyuEnabled,omitempty"`
}

type setConfigResponse struct {
	OK   bool `json:"ok"`
	Data struct {
		Version     int  `json:"version"`
		VolumePct   int  `json:"volumePct"`
		MiyuEnabled bool `json:"miyuEnabled"`
	} `json:"data"`
	Error string `json:"error,omitempty"`
}

func runSetVolume(deviceID *string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		// Parse and clamp pct.
		var pct int
		if _, err := fmt.Sscanf(args[0], "%d", &pct); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "invalid volume value %q: must be an integer 0-100\n", args[0])
			return fmt.Errorf("invalid volume value: %s", args[0])
		}
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}

		id := strings.TrimSpace(*deviceID)
		if id == "" {
			fmt.Fprintln(cmd.ErrOrStderr(), "--device is required")
			return fmt.Errorf("--device is required")
		}

		result, err := postDeviceConfig(cmd, id, setConfigRequest{VolumePct: &pct})
		if err != nil {
			return err
		}

		out, _ := json.Marshal(map[string]any{
			"ok":        true,
			"volumePct": result.Data.VolumePct,
			"version":   result.Data.Version,
		})
		fmt.Println(string(out))
		return nil
	}
}

func newSetMiyuCmd() *cobra.Command {
	var deviceID string

	cmd := &cobra.Command{
		Use:   "set-miyu <on|off>",
		Short: "Toggle 密语模式 (lock-screen voice unlock) via cloud config API",
		Long: `Enable or disable 密语模式 (miyu / lock-screen voice unlock) for a BBClaw device.

When enabled in cloud_saas mode, the device shows the LOCKED page and requires
a spoken passphrase before use. This mirrors the toggle in the web console.

The command reads CLOUD_WS_URL and CLOUD_AUTH_TOKEN from the environment
(or .env file) and calls POST /v1/devices/{deviceId}/config on the cloud.

Examples:
  bbclaw-adapter device set-miyu on  --device <deviceId>
  bbclaw-adapter device set-miyu off --device <deviceId>`,
		Args: cobra.ExactArgs(1),
		RunE: runSetMiyu(&deviceID),
	}

	cmd.Flags().StringVar(&deviceID, "device", "", "Target device ID (required)")
	_ = cmd.MarkFlagRequired("device")

	return cmd
}

func runSetMiyu(deviceID *string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		enabled, err := parseOnOff(args[0])
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "invalid value %q: must be on or off\n", args[0])
			return err
		}

		id := strings.TrimSpace(*deviceID)
		if id == "" {
			fmt.Fprintln(cmd.ErrOrStderr(), "--device is required")
			return fmt.Errorf("--device is required")
		}

		result, err := postDeviceConfig(cmd, id, setConfigRequest{MiyuEnabled: &enabled})
		if err != nil {
			return err
		}

		out, _ := json.Marshal(map[string]any{
			"ok":          true,
			"miyuEnabled": result.Data.MiyuEnabled,
			"version":     result.Data.Version,
		})
		fmt.Println(string(out))
		return nil
	}
}

// parseOnOff parses a human on/off toggle argument into a bool.
func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "1", "enable", "enabled", "yes":
		return true, nil
	case "off", "false", "0", "disable", "disabled", "no":
		return false, nil
	default:
		return false, fmt.Errorf("invalid on/off value: %s", s)
	}
}

// postDeviceConfig sends a partial config update to the cloud for the given
// device and returns the decoded response. Shared by set-volume and set-miyu.
func postDeviceConfig(cmd *cobra.Command, deviceID string, reqBody setConfigRequest) (*setConfigResponse, error) {
	cfg, err := homeadapter.LoadFromEnv()
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "config load failed: %v\n", err)
		return nil, err
	}

	base, err := homeadapter.DeriveCloudHTTPBase(cfg.CloudWSURL)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "cannot derive HTTP base from CLOUD_WS_URL: %v\n", err)
		return nil, err
	}

	body, _ := json.Marshal(reqBody)
	url := base + "/v1/devices/" + deviceID + "/config"
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "构建请求失败: %v\n", err)
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if cfg.CloudAuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.CloudAuthToken)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "请求云端失败: %v\n", err)
		return nil, err
	}
	defer resp.Body.Close()

	var result setConfigResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&result); decErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "解析响应失败 (HTTP %d): %v\n", resp.StatusCode, decErr)
		return nil, decErr
	}

	if !result.OK || resp.StatusCode != http.StatusOK {
		errMsg := result.Error
		if errMsg == "" {
			errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "云端返回错误: %s\n", errMsg)
		return nil, fmt.Errorf("cloud error: %s", errMsg)
	}

	return &result, nil
}
