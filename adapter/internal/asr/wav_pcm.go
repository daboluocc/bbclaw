package asr

import (
	"bytes"
	"encoding/binary"
)

// PCM16LEToWAV wraps raw PCM s16le mono/stereo into a minimal WAV container for upstream APIs that expect .wav.
func PCM16LEToWAV(pcm []byte, sampleRate, channels int) []byte {
	if channels < 1 {
		channels = 1
	}
	bitsPerSample := 16
	blockAlign := channels * bitsPerSample / 8
	byteRate := sampleRate * blockAlign
	dataSize := len(pcm)
	riffSize := 36 + dataSize

	buf := new(bytes.Buffer)
	_, _ = buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(riffSize))
	_, _ = buf.WriteString("WAVE")
	_, _ = buf.WriteString("fmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(buf, binary.LittleEndian, uint16(bitsPerSample))
	_, _ = buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, uint32(dataSize))
	_, _ = buf.Write(pcm)
	return buf.Bytes()
}
