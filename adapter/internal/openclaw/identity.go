package openclaw

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type deviceIdentity struct {
	DeviceID   string
	PublicKey  string
	PrivateKey string
}

type storedDeviceIdentity struct {
	Version     int    `json:"version"`
	DeviceID    string `json:"deviceId"`
	PublicKey   string `json:"publicKey"`
	PrivateKey  string `json:"privateKey"`
	CreatedAtMs int64  `json:"createdAtMs"`
}

func resolveDefaultDeviceIdentityPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "bbclaw", "openclaw-device-identity.json"), nil
}

func loadOrCreateDeviceIdentity(customPath string) (deviceIdentity, error) {
	path := strings.TrimSpace(customPath)
	if path == "" {
		var err error
		path, err = resolveDefaultDeviceIdentityPath()
		if err != nil {
			return deviceIdentity{}, err
		}
	}
	if identity, ok := loadDeviceIdentity(path); ok {
		return identity, nil
	}
	return createAndStoreDeviceIdentity(path)
}

func loadDeviceIdentity(path string) (deviceIdentity, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return deviceIdentity{}, false
	}
	var stored storedDeviceIdentity
	if err := json.Unmarshal(raw, &stored); err != nil {
		return deviceIdentity{}, false
	}
	if stored.Version != 1 {
		return deviceIdentity{}, false
	}
	pub, err := base64.RawURLEncoding.DecodeString(stored.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return deviceIdentity{}, false
	}
	priv, err := base64.RawURLEncoding.DecodeString(stored.PrivateKey)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return deviceIdentity{}, false
	}
	derivedID := deriveDeviceID(pub)
	if strings.TrimSpace(stored.DeviceID) != derivedID {
		return deviceIdentity{}, false
	}
	return deviceIdentity{
		DeviceID:   stored.DeviceID,
		PublicKey:  stored.PublicKey,
		PrivateKey: stored.PrivateKey,
	}, true
}

func createAndStoreDeviceIdentity(path string) (deviceIdentity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return deviceIdentity{}, fmt.Errorf("generate ed25519 keypair: %w", err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	privB64 := base64.RawURLEncoding.EncodeToString(priv)
	deviceID := deriveDeviceID(pub)
	stored := storedDeviceIdentity{
		Version:     1,
		DeviceID:    deviceID,
		PublicKey:   pubB64,
		PrivateKey:  privB64,
		CreatedAtMs: time.Now().UnixMilli(),
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return deviceIdentity{}, fmt.Errorf("mkdir identity dir: %w", err)
	}
	raw, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return deviceIdentity{}, fmt.Errorf("marshal identity: %w", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return deviceIdentity{}, fmt.Errorf("write identity file: %w", err)
	}
	return deviceIdentity{
		DeviceID:   deviceID,
		PublicKey:  pubB64,
		PrivateKey: privB64,
	}, nil
}

func deriveDeviceID(publicKey ed25519.PublicKey) string {
	h := sha256.Sum256(publicKey)
	return hex.EncodeToString(h[:])
}

type deviceAuthPayloadV3 struct {
	deviceID     string
	clientID     string
	clientMode   string
	role         string
	scopes       []string
	signedAtMs   int64
	token        string
	nonce        string
	platform     string
	deviceFamily string
}

func buildDeviceAuthPayloadV3(p deviceAuthPayloadV3) string {
	scopes := strings.Join(p.scopes, ",")
	token := strings.TrimSpace(p.token)
	platform := strings.ToLower(strings.TrimSpace(p.platform))
	deviceFamily := strings.ToLower(strings.TrimSpace(p.deviceFamily))
	return strings.Join([]string{
		"v3",
		p.deviceID,
		p.clientID,
		p.clientMode,
		p.role,
		scopes,
		fmt.Sprintf("%d", p.signedAtMs),
		token,
		p.nonce,
		platform,
		deviceFamily,
	}, "|")
}

func signDevicePayload(privateKeyBase64URL, payload string) string {
	priv, err := base64.RawURLEncoding.DecodeString(privateKeyBase64URL)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return ""
	}
	sig := ed25519.Sign(ed25519.PrivateKey(priv), []byte(payload))
	return base64.RawURLEncoding.EncodeToString(sig)
}
