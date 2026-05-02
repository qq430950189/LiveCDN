package protocol

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// FrameType defines the type of protocol frame
type FrameType byte

const (
	FrameHandshake  FrameType = 0x01
	FrameData       FrameType = 0x02
	FrameHeartbeat  FrameType = 0x03
	FrameClose      FrameType = 0x04
	FrameKeyUpdate  FrameType = 0x05
	FrameWakeup     FrameType = 0x06
	FrameSleep      FrameType = 0x07
)

// Frame is the wire format for our obfuscated protocol
// Format v3: [Magic 2B][Version 1B][Type 1B][Flags 1B][Seq 4B][OriginTS 8B][Len 4B][Payload NB]
// Format v2: [Magic 2B][Type 1B][Flags 1B][Seq 4B][OriginTS 8B][Len 4B][Payload NB] (兼容)
// Format v1: [Magic 2B][Type 1B][Flags 1B][Seq 4B][Len 4B][Payload NB] (兼容)
type Frame struct {
	Version  byte
	Type     FrameType
	Flags    byte
	SeqNum   uint32
	OriginTS uint64 // 推流端原始时间戳 (Unix毫秒)
	Payload  []byte
}

const (
	magicByte1   byte = 0x48 // 'H' - looks like HTTP
	magicByte2   byte = 0x54 // 'T' - looks like HTTP
	ProtoVersion byte = 3    // 当前协议版本
	v3HeaderSize      = 21   // 2+1+1+1+4+8+4
	v2HeaderSize      = 20   // 2+1+1+4+8+4
	v1HeaderSize      = 12   // 2+1+1+4+4
	maxFrameSize      = 1 << 20 // 1MB
)

// MarshalFrame serializes a frame to bytes (v3 format)
func MarshalFrame(f *Frame) ([]byte, error) {
	if len(f.Payload) > maxFrameSize {
		return nil, fmt.Errorf("payload too large: %d", len(f.Payload))
	}
	buf := make([]byte, v3HeaderSize+len(f.Payload))
	buf[0] = magicByte1
	buf[1] = magicByte2
	buf[2] = f.Version
	if buf[2] == 0 {
		buf[2] = ProtoVersion
	}
	buf[3] = byte(f.Type)
	buf[4] = f.Flags
	binary.BigEndian.PutUint32(buf[5:9], f.SeqNum)
	binary.BigEndian.PutUint64(buf[9:17], f.OriginTS)
	binary.BigEndian.PutUint32(buf[17:21], uint32(len(f.Payload)))
	copy(buf[v3HeaderSize:], f.Payload)
	return buf, nil
}

// UnmarshalFrame deserializes a frame from bytes
// 支持 v1/v2/v3 格式自动识别
func UnmarshalFrame(data []byte) (*Frame, error) {
	if len(data) < v1HeaderSize {
		return nil, fmt.Errorf("data too short: %d", len(data))
	}
	if data[0] != magicByte1 || data[1] != magicByte2 {
		return nil, fmt.Errorf("invalid magic bytes")
	}

	// 版本检测: data[2] >= 3 → v3, 否则 → v2/v1
	version := data[2]
	var f *Frame
	var payloadLen uint32
	var hdrSize int

	if version >= 3 {
		// v3: [Magic 2B][Version 1B][Type 1B][Flags 1B][Seq 4B][OriginTS 8B][Len 4B]
		if len(data) < v3HeaderSize {
			return nil, fmt.Errorf("data too short for v3: %d", len(data))
		}
		f = &Frame{
			Version:  version,
			Type:     FrameType(data[3]),
			Flags:    data[4],
			SeqNum:   binary.BigEndian.Uint32(data[5:9]),
			OriginTS: binary.BigEndian.Uint64(data[9:17]),
		}
		payloadLen = binary.BigEndian.Uint32(data[17:21])
		hdrSize = v3HeaderSize
	} else {
		// v2/v1: data[2] 是 Type 而非 Version
		// v2 = 20B (有 OriginTS), v1 = 12B (无 OriginTS)
		// 简化: 假设 v2 格式
		if len(data) < v2HeaderSize {
			// 回退到 v1
			f = &Frame{
				Version: 1,
				Type:    FrameType(data[2]),
				Flags:   data[3],
				SeqNum:  binary.BigEndian.Uint32(data[4:8]),
			}
			payloadLen = binary.BigEndian.Uint32(data[8:12])
			hdrSize = v1HeaderSize
		} else {
			f = &Frame{
				Version:  2,
				Type:     FrameType(data[2]),
				Flags:    data[3],
				SeqNum:   binary.BigEndian.Uint32(data[4:8]),
				OriginTS: binary.BigEndian.Uint64(data[8:16]),
			}
			payloadLen = binary.BigEndian.Uint32(data[16:20])
			hdrSize = v2HeaderSize
		}
	}

	if payloadLen > maxFrameSize {
		return nil, fmt.Errorf("payload too large: %d", payloadLen)
	}
	if uint32(len(data)-hdrSize) < payloadLen {
		return nil, fmt.Errorf("incomplete payload: need %d have %d", payloadLen, len(data)-hdrSize)
	}
	f.Payload = make([]byte, payloadLen)
	copy(f.Payload, data[hdrSize:hdrSize+int(payloadLen)])
	return f, nil
}

// ReadFrame reads a frame from a connection with timeout
func ReadFrame(conn net.Conn, timeout time.Duration) (*Frame, error) {
	if timeout > 0 {
		conn.SetReadDeadline(time.Now().Add(timeout))
	}
	// 先读最小 header (v1 = 12B) 来判断版本
	header := make([]byte, v3HeaderSize)
	if _, err := io.ReadFull(conn, header[:v1HeaderSize]); err != nil {
		return nil, err
	}
	if header[0] != magicByte1 || header[1] != magicByte2 {
		return nil, fmt.Errorf("invalid magic bytes")
	}

	version := header[2]
	var hdrSize int
	var payloadLen uint32

	if version >= 3 {
		// v3: 需要读取完整的 21B header
		if _, err := io.ReadFull(conn, header[v1HeaderSize:v3HeaderSize]); err != nil {
			return nil, err
		}
		hdrSize = v3HeaderSize
		payloadLen = binary.BigEndian.Uint32(header[17:21])
	} else {
		// v2/v1: 根据 data[2] 判断
		// 简化: 假设 v2 (20B header)
		if _, err := io.ReadFull(conn, header[v1HeaderSize:v2HeaderSize]); err != nil {
			// 可能是 v1, 只读了 12B
			hdrSize = v1HeaderSize
			payloadLen = binary.BigEndian.Uint32(header[8:12])
		} else {
			hdrSize = v2HeaderSize
			payloadLen = binary.BigEndian.Uint32(header[16:20])
		}
	}

	if payloadLen > maxFrameSize {
		return nil, fmt.Errorf("payload too large: %d", payloadLen)
	}

	payload := make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err := io.ReadFull(conn, payload); err != nil {
			return nil, err
		}
	}

	// 构造 frame (复用 UnmarshalFrame 的解析逻辑)
	buf := make([]byte, hdrSize+int(payloadLen))
	copy(buf, header[:hdrSize])
	copy(buf[hdrSize:], payload)

	return UnmarshalFrame(buf)
}

// WriteFrame writes a frame to a connection
func WriteFrame(conn net.Conn, f *Frame) error {
	data, err := MarshalFrame(f)
	if err != nil {
		return err
	}
	_, err = conn.Write(data)
	return err
}

// HandshakePayload for initial connection handshake
type HandshakePayload struct {
	NodeID    string `json:"node_id"`
	StreamKey string `json:"stream_key"`
	Token     string `json:"token"`
	Timestamp int64  `json:"ts"`
}

// DataPayload wraps actual stream data
type DataPayload struct {
	Seq      uint32 `json:"seq"`
	Duration float64 `json:"duration,omitempty"` // segment duration in seconds
	Type     string `json:"type"`                // "m3u8" / "ts" / "flv"
	Data     []byte `json:"data"`                // encrypted content
}

// MarshalDataPayload creates a data frame from stream data
func MarshalDataPayload(seq uint32, dataType string, data []byte, duration float64) (*Frame, error) {
	dp := DataPayload{
		Seq:      seq,
		Type:     dataType,
		Data:     data,
		Duration: duration,
	}
	payload, err := json.Marshal(dp)
	if err != nil {
		return nil, err
	}
	return &Frame{
		Type:    FrameData,
		SeqNum:  seq,
		Payload: payload,
	}, nil
}

// ControlMessage for controller <-> agent control channel
type ControlMessage struct {
	Action  string          `json:"action"` // wakeup, sleep, key_update, config_update
	StreamKey string        `json:"stream_key,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
