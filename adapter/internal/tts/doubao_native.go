package tts

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

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var defaultHeader = []byte{0x11, 0x10, 0x11, 0x00}

type Provider interface {
	Synthesize(ctx context.Context, text string) ([]byte, error)
}

type DoubaoNativeProvider struct {
	wsURL   string
	appID   string
	token   string
	cluster string
	voice   string
	dialer  *websocket.Dialer
}

func NewDoubaoNativeProvider(wsURL, appID, token, cluster, voice string) *DoubaoNativeProvider {
	return &DoubaoNativeProvider{
		wsURL:   wsURL,
		appID:   appID,
		token:   token,
		cluster: cluster,
		voice:   voice,
		dialer: &websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
		},
	}
}

func (p *DoubaoNativeProvider) Synthesize(ctx context.Context, text string) ([]byte, error) {
	header := http.Header{"Authorization": []string{fmt.Sprintf("Bearer;%s", p.token)}}
	conn, _, err := p.dialer.DialContext(ctx, p.wsURL, header)
	if err != nil {
		return nil, fmt.Errorf("connect doubao tts ws: %w", err)
	}
	defer conn.Close()

	request := map[string]map[string]any{
		"app": {
			"appid":   p.appID,
			"token":   p.token,
			"cluster": p.cluster,
		},
		"user": {
			"uid": "bbclaw",
		},
		"audio": {
			"voice_type":   p.voice,
			"encoding":     "mp3",
			"speed_ratio":  1.0,
			"volume_ratio": 1.0,
			"pitch_ratio":  1.0,
		},
		"request": {
			"reqid":     uuid.New().String(),
			"text":      text,
			"text_type": "plain",
			"operation": "submit",
		},
	}
	packet, err := buildRequestPacket(request)
	if err != nil {
		return nil, err
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, packet); err != nil {
		return nil, fmt.Errorf("send tts packet: %w", err)
	}

	var output []byte
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("read tts response: %w", err)
		}
		audio, isLast, err := parseTTSResponse(msg)
		if err != nil {
			return nil, err
		}
		output = append(output, audio...)
		if isLast {
			break
		}
	}
	return output, nil
}

func buildRequestPacket(data any) ([]byte, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal tts payload: %w", err)
	}
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("gzip payload: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	compressed := b.Bytes()

	payloadSize := make([]byte, 4)
	binary.BigEndian.PutUint32(payloadSize, uint32(len(compressed)))
	packet := append([]byte{}, defaultHeader...)
	packet = append(packet, payloadSize...)
	packet = append(packet, compressed...)
	return packet, nil
}

func parseTTSResponse(res []byte) ([]byte, bool, error) {
	if len(res) < 4 {
		return nil, false, fmt.Errorf("tts response too short")
	}
	messageType := res[1] >> 4
	flags := res[1] & 0x0f
	headSize := res[0] & 0x0f
	payload := res[headSize*4:]

	switch messageType {
	case 0xb:
		if flags != 0 {
			if len(payload) < 8 {
				return nil, false, fmt.Errorf("tts audio payload too short")
			}
			seq := int32(binary.BigEndian.Uint32(payload[0:4]))
			chunk := payload[8:]
			return chunk, seq < 0, nil
		}
		return nil, false, nil
	case 0xf:
		if len(payload) < 8 {
			return nil, false, fmt.Errorf("tts error payload too short")
		}
		code := int32(binary.BigEndian.Uint32(payload[0:4]))
		errMsg := payload[8:]
		reader, gzErr := gzip.NewReader(bytes.NewReader(errMsg))
		if gzErr == nil {
			if decoded, readErr := io.ReadAll(reader); readErr == nil {
				errMsg = decoded
			}
			_ = reader.Close()
		}
		return nil, false, fmt.Errorf("tts server error [%d]: %s", code, string(errMsg))
	default:
		return nil, false, fmt.Errorf("unknown tts message type: %d", messageType)
	}
}
