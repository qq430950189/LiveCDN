//! 自定义二进制协议
//! 帧格式 v3: [Magic 2B][Version 1B][Type 1B][Flags 1B][Seq 4B][OriginTS 8B][Len 4B][Payload NB]
//! Magic = 0x48 0x54 (仿 HTTP 头部特征)
//! Version = 协议版本号, Controller 维护 N-2 兼容
//! OriginTS = 推流端时间戳 (Unix毫秒), 穿透到客户端用于实时延迟测量

use bytes::{Buf, BufMut, Bytes, BytesMut};
use serde::{Deserialize, Serialize};
use thiserror::Error;

/// 当前协议版本
pub const PROTOCOL_VERSION: u8 = 3;

// 帧类型
#[repr(u8)]
#[derive(Debug, Clone, Copy, PartialEq)]
pub enum FrameType {
    Handshake  = 0x01,
    Data       = 0x02,
    Heartbeat  = 0x03,
    Close      = 0x04,
    KeyUpdate  = 0x05,
    Wakeup     = 0x06,
    Sleep      = 0x07,
}

impl TryFrom<u8> for FrameType {
    type Error = ProtocolError;
    fn try_from(v: u8) -> Result<Self, Self::Error> {
        match v {
            0x01 => Ok(Self::Handshake),
            0x02 => Ok(Self::Data),
            0x03 => Ok(Self::Heartbeat),
            0x04 => Ok(Self::Close),
            0x05 => Ok(Self::KeyUpdate),
            0x06 => Ok(Self::Wakeup),
            0x07 => Ok(Self::Sleep),
            _ => Err(ProtocolError::InvalidFrameType(v)),
        }
    }
}

// 魔术字节 (看起来像 HTTP "HT...")
const MAGIC: [u8; 2] = [0x48, 0x54];
const HEADER_SIZE: usize = 21; // 2+1(v3:Version)+1+1+4+8+4 (v3: 增加 Version 1B)
const MAX_FRAME_SIZE: usize = 1 << 20; // 1MB

#[derive(Error, Debug)]
pub enum ProtocolError {
    #[error("invalid magic bytes: expected HT, got {0:#04x} {1:#04x}")]
    InvalidMagic(u8, u8),
    #[error("unsupported protocol version: {0}, expected {1}")]
    UnsupportedVersion(u8, u8),
    #[error("invalid frame type: {0}")]
    InvalidFrameType(u8),
    #[error("payload too large: {0} bytes")]
    PayloadTooLarge(usize),
    #[error("incomplete frame: need {need} bytes, have {have}")]
    IncompleteFrame { need: usize, have: usize },
    #[error("IO error: {0}")]
    Io(#[from] std::io::Error),
}

/// 协议帧 (v3: 含 Version + Origin Timestamp)
#[derive(Debug, Clone)]
pub struct Frame {
    pub version: u8,
    pub frame_type: FrameType,
    pub flags: u8,
    pub seq: u32,
    /// 推流端原始时间戳 (Unix 毫秒), 客户端用 now - origin_ts 计算端到端延迟
    /// 非 Data 帧时为 0
    pub origin_ts: u64,
    pub payload: Bytes,
}

impl Frame {
    /// 创建新帧
    pub fn new(frame_type: FrameType, seq: u32, payload: impl Into<Bytes>) -> Self {
        Self {
            version: PROTOCOL_VERSION,
            frame_type,
            flags: 0,
            seq,
            origin_ts: 0,
            payload: payload.into(),
        }
    }

    /// 创建带 Origin Timestamp 的数据帧
    pub fn new_with_ts(frame_type: FrameType, seq: u32, origin_ts: u64, payload: impl Into<Bytes>) -> Self {
        Self {
            version: PROTOCOL_VERSION,
            frame_type,
            flags: 0,
            seq,
            origin_ts,
            payload: payload.into(),
        }
    }

    /// 序列化帧为字节
    pub fn encode(&self) -> Bytes {
        let payload_len = self.payload.len();
        let mut buf = BytesMut::with_capacity(HEADER_SIZE + payload_len);

        buf.put_slice(&MAGIC);
        buf.put_u8(self.version);             // v3: Version
        buf.put_u8(self.frame_type as u8);
        buf.put_u8(self.flags);
        buf.put_u32(self.seq);
        buf.put_u64(self.origin_ts);
        buf.put_u32(payload_len as u32);
        buf.put_slice(&self.payload);

        buf.freeze()
    }

    /// 从字节流解码帧
    /// 返回 (Frame, consumed_bytes)
    pub fn decode(data: &[u8]) -> Result<Option<(Self, usize)>, ProtocolError> {
        if data.len() < HEADER_SIZE {
            return Ok(None); // 不够头部
        }

        // 检查魔术字节
        if data[0] != MAGIC[0] || data[1] != MAGIC[1] {
            return Err(ProtocolError::InvalidMagic(data[0], data[1]));
        }

        let version = data[2];
        // 版本兼容: 支持 v2 (无 version 字段) 和 v3
        // v2: [Magic 2B][Type 1B][Flags 1B][Seq 4B][OriginTS 8B][Len 4B] = 20B
        // v3: [Magic 2B][Version 1B][Type 1B][Flags 1B][Seq 4B][OriginTS 8B][Len 4B] = 21B
        if version < 3 {
            // v2 兼容: 把 version 当作 frame_type 解析
            return Self::decode_v2(data);
        }

        let frame_type = FrameType::try_from(data[3])?;
        let flags = data[4];
        let seq = u32::from_be_bytes([data[5], data[6], data[7], data[8]]);
        let origin_ts = u64::from_be_bytes([data[9], data[10], data[11], data[12], data[13], data[14], data[15], data[16]]);
        let payload_len = u32::from_be_bytes([data[17], data[18], data[19], data[20]]) as usize;

        if payload_len > MAX_FRAME_SIZE {
            return Err(ProtocolError::PayloadTooLarge(payload_len));
        }

        let total_len = HEADER_SIZE + payload_len;
        if data.len() < total_len {
            return Ok(None); // 不够完整帧
        }

        let payload = Bytes::copy_from_slice(&data[HEADER_SIZE..total_len]);

        Ok(Some((
            Self {
                version,
                frame_type,
                flags,
                seq,
                origin_ts,
                payload,
            },
            total_len,
        )))
    }

    /// v2 兼容解码 (20B header, 无 version 字段)
    fn decode_v2(data: &[u8]) -> Result<Option<(Self, usize)>, ProtocolError> {
        const V2_HEADER_SIZE: usize = 20;
        if data.len() < V2_HEADER_SIZE {
            return Ok(None);
        }

        let frame_type = FrameType::try_from(data[2])?;
        let flags = data[3];
        let seq = u32::from_be_bytes([data[4], data[5], data[6], data[7]]);
        let origin_ts = u64::from_be_bytes([data[8], data[9], data[10], data[11], data[12], data[13], data[14], data[15]]);
        let payload_len = u32::from_be_bytes([data[16], data[17], data[18], data[19]]) as usize;

        if payload_len > MAX_FRAME_SIZE {
            return Err(ProtocolError::PayloadTooLarge(payload_len));
        }

        let total_len = V2_HEADER_SIZE + payload_len;
        if data.len() < total_len {
            return Ok(None);
        }

        let payload = Bytes::copy_from_slice(&data[V2_HEADER_SIZE..total_len]);

        Ok(Some((
            Self {
                version: 2,
                frame_type,
                flags,
                seq,
                origin_ts,
                payload,
            },
            total_len,
        )))
    }

    /// 创建心跳帧
    pub fn heartbeat(seq: u32) -> Self {
        Self::new(FrameType::Heartbeat, seq, &[][..])
    }

    /// 创建关闭帧
    pub fn close(reason: &str) -> Self {
        Self::new(FrameType::Close, 0, reason.as_bytes().to_vec())
    }
}

// --- 消息类型 ---

#[derive(Serialize, Deserialize, Debug)]
pub struct HandshakeMessage {
    pub stream_key: String,
    pub token: String,
    pub ts: u64,
    pub client_version: String,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct DataMessage {
    pub seq: u32,
    #[serde(rename = "type")]
    pub data_type: String, // "m3u8" | "ts" | "flv"
    #[serde(with = "base64_bytes")]
    pub data: Vec<u8>,
    pub duration: f64, // 秒
}

mod base64_bytes {
    use base64::{engine::general_purpose::STANDARD as BASE64, Engine};
    use serde::{Deserialize, Deserializer, Serializer};

    pub fn serialize<S: Serializer>(data: &Vec<u8>, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(&BASE64.encode(data))
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Vec<u8>, D::Error> {
        let s = String::deserialize(d)?;
        BASE64.decode(&s).map_err(serde::de::Error::custom)
    }
}

#[derive(Serialize, Deserialize, Debug)]
pub struct ControlMessage {
    pub action: String,     // "wakeup" | "sleep" | "key_update" | "config"
    pub stream_key: Option<String>,
    pub payload: Option<serde_json::Value>,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct HeartbeatPayload {
    pub node_id: String,
    pub bw_used: u64,
    pub bw_limit: u64,
    pub online_users: usize,
    pub rtt_ms: u32,
    pub loss_rate: f64,
    pub version: String,
    // Mesh 级联心跳字段
    #[serde(default)]
    pub cascade_depth: i32,
    #[serde(default)]
    pub children_count: i32,
    #[serde(default)]
    pub cascade_egress_bw: i64,
    #[serde(default)]
    pub current_upstream: String,
    #[serde(default)]
    pub stream_lag_ms: i32,
}

#[derive(Serialize, Deserialize, Debug)]
pub struct RegisterPayload {
    pub node_id: String,
    pub public_ip: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub public_ipv6: String,
    pub port: u16,
    pub region: String,
    pub isp: String,
    pub bw_limit: u64,
    pub domain: String,
    pub protocol: String,
    pub ws_path: String,
    pub tls_enabled: bool,
    pub token: String,
    #[serde(default)]
    pub cascade_enabled: bool,
    #[serde(default)]
    pub cascade_upstream: bool,
}

// --- 帧编解码器 (用于 TCP/WebSocket 流) ---

/// 增量帧解码器 - 处理粘包/拆包
pub struct FrameDecoder {
    buffer: BytesMut,
}

impl FrameDecoder {
    pub fn new() -> Self {
        Self {
            buffer: BytesMut::with_capacity(64 * 1024),
        }
    }

    /// 追加数据到内部缓冲区
    pub fn feed(&mut self, data: &[u8]) {
        self.buffer.extend_from_slice(data);
    }

    /// 尝试从缓冲区解码一个完整帧
    pub fn decode(&mut self) -> Result<Option<Frame>, ProtocolError> {
        match Frame::decode(&self.buffer)? {
            Some((frame, consumed)) => {
                self.buffer.advance(consumed);
                Ok(Some(frame))
            }
            None => Ok(None),
        }
    }

    /// 缓冲区长度
    pub fn buffer_len(&self) -> usize {
        self.buffer.len()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_frame_encode_decode_v3() {
        let payload = Bytes::from_static(b"hello world");
        let original = Frame::new(FrameType::Data, 42, payload);
        let encoded = original.encode();

        // v3 header = 21 bytes
        assert_eq!(encoded[2], PROTOCOL_VERSION);

        let (decoded, consumed) = Frame::decode(&encoded).unwrap().unwrap();
        assert_eq!(consumed, encoded.len());
        assert_eq!(decoded.version, PROTOCOL_VERSION);
        assert_eq!(decoded.frame_type, FrameType::Data);
        assert_eq!(decoded.seq, 42);
        assert_eq!(&decoded.payload[..], b"hello world");
    }

    #[test]
    fn test_frame_with_origin_ts() {
        let frame = Frame::new_with_ts(FrameType::Data, 1, 1700000000000u64, Bytes::from_static(b"test"));
        let encoded = frame.encode();
        let (decoded, _) = Frame::decode(&encoded).unwrap().unwrap();
        assert_eq!(decoded.origin_ts, 1700000000000u64);
    }

    #[test]
    fn test_frame_decoder_incremental() {
        let mut decoder = FrameDecoder::new();

        let frame1 = Frame::new(FrameType::Data, 1, Bytes::from_static(b"first"));
        let frame2 = Frame::new(FrameType::Data, 2, Bytes::from_static(b"second"));

        let encoded1 = frame1.encode();
        let encoded2 = frame2.encode();
        let mut combined = Vec::with_capacity(encoded1.len() + encoded2.len());
        combined.extend_from_slice(&encoded1);
        combined.extend_from_slice(&encoded2);

        // 逐字节喂入
        for i in 0..combined.len() {
            decoder.feed(&combined[i..i + 1]);
        }

        let decoded1 = decoder.decode().unwrap().unwrap();
        assert_eq!(decoded1.seq, 1);

        let decoded2 = decoder.decode().unwrap().unwrap();
        assert_eq!(decoded2.seq, 2);

        assert!(decoder.decode().unwrap().is_none());
    }

    #[test]
    fn test_heartbeat_frame() {
        let frame = Frame::heartbeat(99);
        assert_eq!(frame.frame_type, FrameType::Heartbeat);
        assert_eq!(frame.seq, 99);
        assert!(frame.payload.is_empty());
    }

    #[test]
    fn test_invalid_magic() {
        // v3 header is 21 bytes, need enough data to reach magic check
        let data = [0xFFu8, 0xFF, 0x03, 0x01, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0];
        let result = Frame::decode(&data);
        assert!(result.is_err());
    }
}
