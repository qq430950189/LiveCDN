//! FLV 输出模块
//! 将 FlvTagPacket 序列化为 FLV 线路格式, 供 HTTP-FLV 和 WS-FLV 使用
//!
//! FLV 线路格式:
//!   [FLV Header 9B][PrevTagSize0 4B]
//!   [Tag: type 1B + datasize 3B + ts 3B + tsExt 1B + streamID 3B + data NB + PrevTagSize 4B]

use super::flv::FlvTagPacket;

#[cfg(test)]
use super::flv::FlvTagType;

/// 构建 FLV 文件头 (9B header + 4B PrevTagSize0 = 13B)
pub fn build_flv_header() -> Vec<u8> {
    let mut buf = Vec::with_capacity(13);
    // FLV signature
    buf.extend_from_slice(b"FLV");
    // Version 1
    buf.push(0x01);
    // Flags: audio(0x04) + video(0x01) = 0x05
    buf.push(0x05);
    // Header size (9)
    buf.extend_from_slice(&9u32.to_be_bytes());
    // PreviousTagSize0 (0)
    buf.extend_from_slice(&0u32.to_be_bytes());
    buf
}

/// 将 FlvTagPacket 序列化为 FLV 线路格式字节
/// 输出: [tag_header 11B][tag_data NB][prev_tag_size 4B]
pub fn build_flv_tag_wire(tag: &FlvTagPacket) -> Vec<u8> {
    let data_size = tag.data.len() as u32;
    let ts = tag.timestamp_ms;
    let total = 11 + data_size as usize + 4;

    let mut buf = Vec::with_capacity(total);

    // Tag type
    buf.push(tag.tag_type as u8);

    // Data size (3B big-endian)
    buf.push((data_size >> 16) as u8);
    buf.push((data_size >> 8) as u8);
    buf.push(data_size as u8);

    // Timestamp lower 24 bits (3B big-endian)
    buf.push((ts >> 16) as u8);
    buf.push((ts >> 8) as u8);
    buf.push(ts as u8);

    // Timestamp extended (upper 8 bits)
    buf.push((ts >> 24) as u8);

    // Stream ID (3B, always 0)
    buf.push(0);
    buf.push(0);
    buf.push(0);

    // Tag data
    buf.extend_from_slice(&tag.data);

    // Previous tag size = tag_header(11) + data_size
    let prev = 11 + data_size;
    buf.extend_from_slice(&prev.to_be_bytes());

    buf
}

#[cfg(test)]
mod tests {
    use super::*;
    use bytes::Bytes;

    #[test]
    fn test_flv_header() {
        let header = build_flv_header();
        assert_eq!(header.len(), 13);
        assert_eq!(&header[0..3], b"FLV");
        assert_eq!(header[3], 1); // version
        assert_eq!(header[4], 5); // flags
        // Header size = 9
        assert_eq!(u32::from_be_bytes(header[5..9].try_into().unwrap()), 9);
        // PrevTagSize0 = 0
        assert_eq!(u32::from_be_bytes(header[9..13].try_into().unwrap()), 0);
    }

    #[test]
    fn test_flv_tag_wire_video() {
        let tag = FlvTagPacket {
            tag_type: FlvTagType::Video,
            timestamp_ms: 1234,
            data: Bytes::from_static(b"fake_video_data"),
            origin_ts: 0,
        };

        let wire = build_flv_tag_wire(&tag);

        // Tag type
        assert_eq!(wire[0], 0x09);

        // Data size = 15 ("fake_video_data")
        let data_size = ((wire[1] as u32) << 16) | ((wire[2] as u32) << 8) | wire[3] as u32;
        assert_eq!(data_size, 15);

        // Timestamp = 1234
        let ts = ((wire[4] as u32) << 16) | ((wire[5] as u32) << 8) | wire[6] as u32
            | ((wire[7] as u32) << 24);
        assert_eq!(ts, 1234);

        // Stream ID = 0
        assert_eq!(wire[8], 0);
        assert_eq!(wire[9], 0);
        assert_eq!(wire[10], 0);

        // Data
        assert_eq!(&wire[11..26], b"fake_video_data");

        // Prev tag size = 11 + 15 = 26
        let prev = u32::from_be_bytes(wire[26..30].try_into().unwrap());
        assert_eq!(prev, 26);
    }

    #[test]
    fn test_flv_tag_wire_large_timestamp() {
        // Timestamp > 0xFFFFFF (需要 ts_ext)
        let ts: u32 = 0x01_00_1234; // 16781300 ms
        let tag = FlvTagPacket {
            tag_type: FlvTagType::Audio,
            timestamp_ms: ts,
            data: Bytes::from_static(b"audio"),
            origin_ts: 0,
        };

        let wire = build_flv_tag_wire(&tag);

        // Lower 24 bits
        let ts_lower = ((wire[4] as u32) << 16) | ((wire[5] as u32) << 8) | wire[6] as u32;
        // Upper 8 bits (ts_ext)
        let ts_ext = wire[7] as u32;

        assert_eq!(ts_lower, ts & 0xFFFFFF);
        assert_eq!(ts_ext, (ts >> 24) & 0xFF);
    }
}
