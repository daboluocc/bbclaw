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

type setConfigRequest struct {
	VolumePct int `json:"volumePct"`
}

type setConfigResponse struct {
	OK   bool `json:"ok"`
	Data struct {
		Version   int `json:"version"`
		VolumePct int `json:"volumePct"`
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

		// Load cloud config from env.
		cfg, err := homeadapter.LoadFromEnv()
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "config load failed: %v\n", err)
			return err
		}

		base, err := homeadapter.DeriveCloudHTTPBase(cfg.CloudWSURL)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "cannot derive HTTP base from CLOUD_WS_URL: %v\n", err)
			return err
		}

		// Build request.
		body, _ := json.Marshal(setConfigRequest{VolumePct: pct})
		url := base + "/v1/devices/" + id + "/config"
		httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "构建请求失败: %v\n", err)
			return err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if cfg.CloudAuthToken != "" {
			httpReq.Header.Set("Authorization", "Bearer "+cfg.CloudAuthToken)
		}

		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(httpReq)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "请求云端失败: %v\n", err)
			return err
		}
		defer resp.Body.Close()

		var result setConfigResponse
		if decErr := json.NewDecoder(resp.Body).Decode(&result); decErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "解析响应失败 (HTTP %d): %v\n", resp.StatusCode, decErr)
			return decErr
		}

		if !result.OK || resp.StatusCode != http.StatusOK {
			errMsg := result.Error
			if errMsg == "" {
				errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "云端返回错误: %s\n", errMsg)
			return fmt.Errorf("cloud error: %s", errMsg)
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
