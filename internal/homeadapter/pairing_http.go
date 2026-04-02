package homeadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DeriveCloudHTTPBase turns ws(s)://host:port/path into http(s)://host:port for REST calls.
func DeriveCloudHTTPBase(wsURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(wsURL))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported CLOUD_WS_URL scheme: %s", u.Scheme)
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

type pairingHTTPData struct {
	Status     string `json:"status"`
	Code       string `json:"code"`
	ExpiresAt  string `json:"expiresAt"`
	HomeSiteID string `json:"homeSiteId"`
}

type pairingHTTPEnvelope struct {
	OK   bool            `json:"ok"`
	Data pairingHTTPData `json:"data"`
}

func (a *Adapter) pairingPollLoop(ctx context.Context) {
	base, err := DeriveCloudHTTPBase(a.cfg.CloudWSURL)
	if err != nil {
		a.log.Warnf("home-adapter pairing HTTP poll disabled: %v", err)
		return
	}
	client := &http.Client{Timeout: a.cfg.HTTPTimeout}
	delay := 25 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if a.pollHomePairingOnce(ctx, client, base) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (a *Adapter) pollHomePairingOnce(ctx context.Context, client *http.Client, base string) bool {
	body, err := json.Marshal(map[string]string{"homeSiteId": a.cfg.HomeSiteID})
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/pairings/request", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.CloudAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.CloudAuthToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		a.log.Warnf("home-adapter pairing poll request failed err=%v", err)
		return false
	}
	defer resp.Body.Close()
	var env pairingHTTPEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		a.log.Warnf("home-adapter pairing poll decode err=%v", err)
		return false
	}
	if !env.OK || resp.StatusCode != http.StatusOK {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(env.Data.Status)) {
	case "approved":
		a.log.Infof("home-adapter registration approved home_site=%s (HTTP /v1/pairings/request)", a.cfg.HomeSiteID)
		return true
	case "claim_required":
		a.announceRegistrationCode("http", env.Data.Code, env.Data.ExpiresAt)
		return false
	default:
		return false
	}
}
