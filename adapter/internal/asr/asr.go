package asr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
)

type Metadata struct {
	DeviceID   string
	SessionKey string
	StreamID   string
	Codec      string
	SampleRate int
	Channels   int
}

type Segment struct {
	StartMs int64  `json:"startMs"`
	EndMs   int64  `json:"endMs"`
	Text    string `json:"text"`
}

type Result struct {
	Text       string    `json:"text"`
	Segments   []Segment `json:"segments,omitempty"`
	DurationMs int64     `json:"durationMs"`
}

type Provider interface {
	Transcribe(ctx context.Context, audio []byte, meta Metadata) (Result, error)
}

// ReadinessProbe is implemented by concrete ASR providers for startup connectivity checks.
type ReadinessProbe interface {
	Ping(ctx context.Context) error
}

type APIError struct {
	Code       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("asr error: code=%s status=%d message=%s", e.Code, e.StatusCode, e.Message)
}

type OpenAICompatibleProvider struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

func NewOpenAICompatibleProvider(baseURL, apiKey, model string, httpClient *http.Client) *OpenAICompatibleProvider {
	return &OpenAICompatibleProvider{
		baseURL: strings.TrimSpace(baseURL),
		apiKey:  strings.TrimSpace(apiKey),
		model:   strings.TrimSpace(model),
		http:    httpClient,
	}
}

// Ping checks that the OpenAI-compatible HTTP API is reachable (GET /v1/models).
func (p *OpenAICompatibleProvider) Ping(ctx context.Context) error {
	endpoint, err := joinURL(p.baseURL, "/v1/models")
	if err != nil {
		return fmt.Errorf("asr readiness: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("asr readiness: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("asr readiness: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("asr readiness: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (p *OpenAICompatibleProvider) Transcribe(ctx context.Context, audio []byte, meta Metadata) (Result, error) {
	if len(audio) == 0 {
		return Result{}, &APIError{Code: "ASR_BAD_REQUEST", Message: "empty audio"}
	}

	payload := audio
	if meta.SampleRate > 0 && meta.Channels > 0 {
		payload = PCM16LEToWAV(audio, meta.SampleRate, meta.Channels)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", p.model); err != nil {
		return Result{}, fmt.Errorf("write model field: %w", err)
	}
	fileWriter, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		return Result{}, fmt.Errorf("create file field: %w", err)
	}
	if _, err := fileWriter.Write(payload); err != nil {
		return Result{}, fmt.Errorf("write file field: %w", err)
	}
	if err := writer.Close(); err != nil {
		return Result{}, fmt.Errorf("close multipart writer: %w", err)
	}

	endpoint, err := joinURL(p.baseURL, "/v1/audio/transcriptions")
	if err != nil {
		return Result{}, fmt.Errorf("build asr endpoint: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return Result{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("send asr request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("read asr response: %w", err)
	}

	if resp.StatusCode >= 400 {
		code := "ASR_FAILED"
		if resp.StatusCode == http.StatusTooManyRequests {
			code = "ASR_RATE_LIMITED"
		}
		return Result{}, &APIError{Code: code, StatusCode: resp.StatusCode, Message: string(respBody)}
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Result{}, fmt.Errorf("decode asr response: %w", err)
	}
	if strings.TrimSpace(parsed.Text) == "" {
		return Result{}, &APIError{Code: "ASR_EMPTY_TRANSCRIPT", StatusCode: resp.StatusCode, Message: "empty text"}
	}

	return Result{Text: parsed.Text}, nil
}

func joinURL(base, pathPart string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = path.Join(strings.TrimSuffix(u.Path, "/"), pathPart)
	return u.String(), nil
}
