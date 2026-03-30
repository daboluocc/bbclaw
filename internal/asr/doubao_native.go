package asr

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

const (
	clientFullRequest  = 0x1
	clientAudioRequest = 0x2
	serverFullResponse = 0x9

	noSequence  = 0x0
	negSequence = 0x2
)

type DoubaoNativeProvider struct {
	wsURL       string
	appID       string
	accessToken string
	resourceID  string
	modelName   string
	language    string
	dialer      *websocket.Dialer
}

// Ping opens a WebSocket to the ASR endpoint and closes immediately (handshake + auth check).
func (p *DoubaoNativeProvider) Ping(ctx context.Context) error {
	headers := http.Header{
		"X-Api-App-Key":     []string{p.appID},
		"X-Api-Access-Key":  []string{p.accessToken},
		"X-Api-Resource-Id": []string{p.resourceID},
		"X-Api-Connect-Id":  []string{fmt.Sprintf("%d", time.Now().UnixNano())},
	}
	conn, resp, err := p.dialer.DialContext(ctx, p.wsURL, headers)
	if err != nil {
		if resp != nil {
			return &APIError{
				Code:       "ASR_CONNECT_FAILED",
				StatusCode: resp.StatusCode,
				Message:    err.Error(),
			}
		}
		return fmt.Errorf("asr readiness ws dial: %w", err)
	}
	_ = conn.Close()
	return nil
}

func NewDoubaoNativeProvider(wsURL, appID, accessToken, resourceID, modelName, language string) *DoubaoNativeProvider {
	return &DoubaoNativeProvider{
		wsURL:       wsURL,
		appID:       appID,
		accessToken: accessToken,
		resourceID:  resourceID,
		modelName:   modelName,
		language:    language,
		dialer: &websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
		},
	}
}

func (p *DoubaoNativeProvider) Transcribe(ctx context.Context, audio []byte, _ Metadata) (Result, error) {
	headers := http.Header{
		"X-Api-App-Key":     []string{p.appID},
		"X-Api-Access-Key":  []string{p.accessToken},
		"X-Api-Resource-Id": []string{p.resourceID},
		"X-Api-Connect-Id":  []string{fmt.Sprintf("%d", time.Now().UnixNano())},
	}

	conn, resp, err := p.dialer.DialContext(ctx, p.wsURL, headers)
	if err != nil {
		if resp != nil {
			return Result{}, &APIError{
				Code:       "ASR_CONNECT_FAILED",
				StatusCode: resp.StatusCode,
				Message:    err.Error(),
			}
		}
		return Result{}, fmt.Errorf("dial doubao ws: %w", err)
	}
	defer conn.Close()

	initPacket, err := p.buildInitialPacket()
	if err != nil {
		return Result{}, err
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, initPacket); err != nil {
		return Result{}, fmt.Errorf("write init packet: %w", err)
	}

	_, initResponse, err := conn.ReadMessage()
	if err != nil {
		return Result{}, fmt.Errorf("read init response: %w", err)
	}
	if _, _, err := parseDoubaoResponse(initResponse); err != nil {
		return Result{}, fmt.Errorf("parse init response: %w", err)
	}

	audioPacket, err := buildAudioPacket(audio, true)
	if err != nil {
		return Result{}, err
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, audioPacket); err != nil {
		return Result{}, fmt.Errorf("write audio packet: %w", err)
	}

	var finalText string
	for {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		default:
		}
		if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return Result{}, fmt.Errorf("set read deadline: %w", err)
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return Result{}, fmt.Errorf("read asr response: %w", err)
		}
		payload, isLast, err := parseDoubaoResponse(msg)
		if err != nil {
			return Result{}, err
		}
		if payload != nil {
			if resultMap, ok := payload["result"].(map[string]any); ok {
				if text, ok := resultMap["text"].(string); ok && text != "" {
					finalText = text
				}
			}
		}
		if isLast {
			break
		}
	}
	if finalText == "" {
		return Result{}, &APIError{Code: "ASR_EMPTY_TRANSCRIPT", Message: "empty transcript"}
	}
	return Result{Text: finalText}, nil
}

func (p *DoubaoNativeProvider) buildInitialPacket() ([]byte, error) {
	req := map[string]any{
		"user": map[string]any{
			"uid": fmt.Sprintf("%d", time.Now().UnixNano()),
		},
		"audio": map[string]any{
			"format":   "pcm",
			"rate":     16000,
			"bits":     16,
			"channel":  1,
			"language": p.language,
		},
		"request": map[string]any{
			"model_name":      p.modelName,
			"end_window_size": 300,
			"enable_punc":     true,
			"enable_itn":      true,
			"enable_ddc":      false,
			"result_type":     "single",
			"show_utterances": false,
		},
	}

	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal init request: %w", err)
	}
	zipped, err := gzipBytes(raw)
	if err != nil {
		return nil, err
	}
	header := generateHeader(clientFullRequest, noSequence, 0x1)
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, uint32(len(zipped)))
	packet := append(header, size...)
	packet = append(packet, zipped...)
	return packet, nil
}

func buildAudioPacket(audio []byte, isLast bool) ([]byte, error) {
	zipped, err := gzipBytes(audio)
	if err != nil {
		return nil, err
	}
	flags := byte(noSequence)
	if isLast {
		flags = negSequence
	}
	header := generateHeader(clientAudioRequest, flags, 0x0)
	size := make([]byte, 4)
	binary.BigEndian.PutUint32(size, uint32(len(zipped)))
	packet := append(header, size...)
	packet = append(packet, zipped...)
	return packet, nil
}

func generateHeader(messageType, flags, serialization byte) []byte {
	return []byte{
		(1 << 4) | 1,
		(messageType << 4) | flags,
		(serialization << 4) | 0x1,
		0,
	}
}

func gzipBytes(raw []byte) ([]byte, error) {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return b.Bytes(), nil
}

func parseDoubaoResponse(data []byte) (map[string]any, bool, error) {
	if len(data) < 4 {
		return nil, false, fmt.Errorf("response too short")
	}
	headerSize := data[0] & 0x0f
	messageType := data[1] >> 4
	flags := data[1] & 0x0f
	serializationMethod := data[2] >> 4
	compressionMethod := data[2] & 0x0f

	payload := data[int(headerSize)*4:]
	if len(payload) == 0 {
		return nil, flags&0x02 != 0, nil
	}

	if messageType != serverFullResponse {
		return nil, flags&0x02 != 0, nil
	}

	if len(payload) >= 8 && payload[0] != '{' {
		// Sequence + payload size
		payloadSize := int(binary.BigEndian.Uint32(payload[4:8]))
		if len(payload) >= 8+payloadSize {
			payload = payload[8 : 8+payloadSize]
		}
	}

	if compressionMethod == 0x1 {
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err == nil {
			defer reader.Close()
			unzipped, readErr := io.ReadAll(reader)
			if readErr == nil {
				payload = unzipped
			}
		}
	}

	if serializationMethod != 0x1 {
		return nil, flags&0x02 != 0, nil
	}

	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, false, fmt.Errorf("decode response json: %w", err)
	}
	return out, flags&0x02 != 0, nil
}
