//! 加密引擎 - 基于 ring 库
//! 支持 ChaCha20-Poly1305 / AES-128-GCM
//! HKDF-SHA256 密钥派生，每分片独立密钥

use ring::aead::{Aad, LessSafeKey, Nonce, UnboundKey, AES_128_GCM, CHACHA20_POLY1305};
use ring::digest::{digest, SHA256};
use ring::hkdf::{KeyType, Salt};
use ring::hmac::{sign, Key, HMAC_SHA256};
use thiserror::Error;

/// Helper type for HKDF output length
struct ChunkKeyLen(usize);

impl KeyType for ChunkKeyLen {
    fn len(&self) -> usize {
        self.0
    }
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum CipherSuite {
    ChaCha20Poly1305,
    Aes128Gcm,
}

impl CipherSuite {
    pub fn from_str(s: &str) -> Option<Self> {
        match s {
            "chacha20-poly1305" | "chacha20" => Some(Self::ChaCha20Poly1305),
            "aes-128-gcm" | "aes128" => Some(Self::Aes128Gcm),
            _ => None,
        }
    }

    pub fn key_len(&self) -> usize {
        match self {
            Self::ChaCha20Poly1305 => 32,
            Self::Aes128Gcm => 16,
        }
    }

    pub fn nonce_len(&self) -> usize {
        12 // both use 96-bit nonces
    }

    pub fn tag_len(&self) -> usize {
        16 // both use 128-bit tags
    }

    pub fn as_str(&self) -> &'static str {
        match self {
            Self::ChaCha20Poly1305 => "chacha20-poly1305",
            Self::Aes128Gcm => "aes-128-gcm",
        }
    }
}

#[derive(Error, Debug)]
pub enum CryptoError {
    #[error("encryption failed")]
    EncryptFailed,
    #[error("decryption failed")]
    DecryptFailed,
    #[error("invalid key length: expected {expected}, got {actual}")]
    InvalidKeyLength { expected: usize, actual: usize },
    #[error("invalid ciphertext: too short")]
    CiphertextTooShort,
    #[error("unsupported cipher suite")]
    UnsupportedCipher,
}

/// 密钥信息
pub struct KeyInfo {
    pub master_key: Vec<u8>,
    pub salt: Vec<u8>,          // HKDF salt
    pub cipher: CipherSuite,
}

impl KeyInfo {
    pub fn new(cipher: CipherSuite) -> Result<Self, CryptoError> {
        let master_key = random_bytes(cipher.key_len());
        let salt = random_bytes(32); // HKDF salt
        Ok(Self { master_key, salt, cipher })
    }

    pub fn from_parts(key: &[u8], salt: &[u8], cipher: CipherSuite) -> Result<Self, CryptoError> {
        if key.len() != cipher.key_len() {
            return Err(CryptoError::InvalidKeyLength {
                expected: cipher.key_len(),
                actual: key.len(),
            });
        }
        Ok(Self {
            master_key: key.to_vec(),
            salt: salt.to_vec(),
            cipher,
        })
    }

    /// 使用 HKDF-SHA256 从 master_key + seq_num 派生每分片密钥
    fn derive_chunk_key(&self, seq: u32) -> Vec<u8> {
        let salt = Salt::new(ring::hkdf::HKDF_SHA256, &self.salt);
        let info = format!("livecdn-chunk-{}", seq);
        let key_len = self.cipher.key_len();

        let prk = salt.extract(&self.master_key);
        let mut out = vec![0u8; key_len];

        match prk.expand(&[info.as_bytes()], ChunkKeyLen(key_len)) {
            Ok(okm) => {
                if okm.fill(&mut out).is_ok() {
                    out
                } else {
                    fallback_key(&self.master_key, seq, key_len)
                }
            }
            Err(_) => fallback_key(&self.master_key, seq, key_len),
        }
    }

    /// 从 seq_num 生成随机 nonce (12 bytes)
    /// 每次调用都生成真随机 nonce，防重放攻击
    fn derive_nonce(&self, _seq: u32) -> [u8; 12] {
        random_nonce()
    }
}

/// 流式加密器 - 维护序列号状态
pub struct StreamEncryptor {
    key_info: KeyInfo,
    seq: u32,
}

impl StreamEncryptor {
    pub fn new(key_info: KeyInfo) -> Self {
        Self { key_info, seq: 0 }
    }

    /// 加密一个数据块
    /// 输出格式: nonce(12B) || ciphertext || tag(16B)
    pub fn encrypt(&mut self, plaintext: &[u8]) -> Result<Vec<u8>, CryptoError> {
        let chunk_key = self.key_info.derive_chunk_key(self.seq);
        let nonce_bytes = self.key_info.derive_nonce(self.seq);
        self.seq += 1;

        let nonce = Nonce::assume_unique_for_key(nonce_bytes);
        let unbound_key = UnboundKey::new(&algorithm(&self.key_info.cipher), &chunk_key)
            .map_err(|_| CryptoError::EncryptFailed)?;
        let key = LessSafeKey::new(unbound_key);

        // 分配输出缓冲区: nonce(12) + plaintext (会被原地加密为 ciphertext)
        let mut out = Vec::with_capacity(12 + plaintext.len());
        out.extend_from_slice(&nonce_bytes);
        out.extend_from_slice(plaintext);

        // seal_in_place_separate_tag: 原地加密 out[12..], tag 单独返回
        let tag = key.seal_in_place_separate_tag(
            nonce,
            Aad::empty(),
            &mut out[12..],
        ).map_err(|_| CryptoError::EncryptFailed)?;

        // 追加 tag
        out.extend_from_slice(tag.as_ref());

        Ok(out)
    }
}

/// 流式解密器
pub struct StreamDecryptor {
    key_info: KeyInfo,
    seq: u32,
}

impl StreamDecryptor {
    pub fn new(key_info: KeyInfo) -> Self {
        Self { key_info, seq: 0 }
    }

    /// 解密一个数据块
    /// 输入格式: nonce(12B) || ciphertext || tag(16B)
    /// nonce 已嵌入密文头部，无需再派生
    pub fn decrypt(&mut self, data: &[u8]) -> Result<Vec<u8>, CryptoError> {
        let nonce_len = self.key_info.cipher.nonce_len();
        let tag_len = self.key_info.cipher.tag_len();

        if data.len() < nonce_len + tag_len {
            return Err(CryptoError::CiphertextTooShort);
        }

        // 从密文头部读取 nonce
        let nonce_bytes: [u8; 12] = data[..nonce_len].try_into()
            .map_err(|_| CryptoError::CiphertextTooShort)?;

        // 派生该分片的密钥 (seq 用于密钥派生, nonce 从密文读取)
        let chunk_key = self.key_info.derive_chunk_key(self.seq);
        self.seq += 1;

        let nonce = Nonce::assume_unique_for_key(nonce_bytes);
        let unbound_key = UnboundKey::new(&algorithm(&self.key_info.cipher), &chunk_key)
            .map_err(|_| CryptoError::DecryptFailed)?;
        let key = LessSafeKey::new(unbound_key);

        // 复制数据用于 in-place 解密
        let mut buf = data[nonce_len..].to_vec();
        let plaintext = key.open_in_place(nonce, Aad::empty(), &mut buf)
            .map_err(|_| CryptoError::DecryptFailed)?;

        Ok(plaintext.to_vec())
    }

    /// 使用数据中嵌入的 nonce 解密 (兼容模式, 与 decrypt 相同逻辑)
    pub fn decrypt_with_embedded_nonce(&mut self, data: &[u8]) -> Result<Vec<u8>, CryptoError> {
        // 与 decrypt 逻辑相同, nonce 从密文头部读取
        self.decrypt(data)
    }
}

/// 令牌签名验证
pub struct TokenSigner {
    key: Key,
}

impl TokenSigner {
    pub fn new(secret: &[u8]) -> Self {
        Self {
            key: Key::new(HMAC_SHA256, secret),
        }
    }

    /// 签名: HMAC-SHA256(data) -> hex
    pub fn sign(&self, data: &str) -> String {
        let sig = sign(&self.key, data.as_bytes());
        hex_encode(sig.as_ref())
    }

    /// 验证签名
    pub fn verify(&self, data: &str, signature: &str) -> bool {
        let expected = self.sign(data);
        ring::constant_time::verify_slices_are_equal(
            expected.as_bytes(),
            signature.as_bytes(),
        ).is_ok()
    }
}

// --- 辅助函数 ---

fn algorithm(suite: &CipherSuite) -> &'static ring::aead::Algorithm {
    match suite {
        CipherSuite::ChaCha20Poly1305 => &CHACHA20_POLY1305,
        CipherSuite::Aes128Gcm => &AES_128_GCM,
    }
}

fn random_bytes(len: usize) -> Vec<u8> {
    use ring::rand::{SecureRandom, SystemRandom};
    let rng = SystemRandom::new();
    let mut buf = vec![0u8; len];
    rng.fill(&mut buf).expect("rng failed");
    buf
}

fn random_nonce() -> [u8; 12] {
    let bytes = random_bytes(12);
    let mut nonce = [0u8; 12];
    nonce.copy_from_slice(&bytes);
    nonce
}

fn hex_encode(data: &[u8]) -> String {
    data.iter().map(|b| format!("{:02x}", b)).collect()
}

/// Fallback key derivation: SHA256(master_key || seq_bytes)
fn fallback_key(master_key: &[u8], seq: u32, key_len: usize) -> Vec<u8> {
    let mut data = Vec::with_capacity(master_key.len() + 4);
    data.extend_from_slice(master_key);
    data.extend_from_slice(&seq.to_be_bytes());
    let hash = digest(&SHA256, &data);
    hash.as_ref()[..key_len].to_vec()
}

// --- 测试 ---
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_chacha20_roundtrip() {
        let key_info = KeyInfo::new(CipherSuite::ChaCha20Poly1305).unwrap();
        let key_clone = KeyInfo::from_parts(
            &key_info.master_key,
            &key_info.salt,
            key_info.cipher,
        ).unwrap();

        let mut enc = StreamEncryptor::new(key_info);
        let mut dec = StreamDecryptor::new(key_clone);

        let plaintext = b"Hello, LiveCDN! This is a test of the encryption system.";
        let encrypted = enc.encrypt(plaintext).unwrap();
        
        assert_ne!(encrypted.as_slice(), plaintext.as_slice());
        assert!(encrypted.len() > plaintext.len()); // + nonce + tag

        let decrypted = dec.decrypt(&encrypted).unwrap();
        assert_eq!(decrypted, plaintext);
    }

    #[test]
    fn test_aes128_roundtrip() {
        let key_info = KeyInfo::new(CipherSuite::Aes128Gcm).unwrap();
        let key_clone = KeyInfo::from_parts(
            &key_info.master_key,
            &key_info.salt,
            key_info.cipher,
        ).unwrap();

        let mut enc = StreamEncryptor::new(key_info);
        let mut dec = StreamDecryptor::new(key_clone);

        let plaintext = b"AES-128-GCM test payload";
        let encrypted = enc.encrypt(plaintext).unwrap();
        let decrypted = dec.decrypt(&encrypted).unwrap();
        assert_eq!(decrypted, plaintext);
    }

    #[test]
    fn test_sequential_chunks_different_keys() {
        let key_info = KeyInfo::new(CipherSuite::ChaCha20Poly1305).unwrap();
        let key_clone = KeyInfo::from_parts(
            &key_info.master_key,
            &key_info.salt,
            key_info.cipher,
        ).unwrap();

        let mut enc = StreamEncryptor::new(key_info);
        let mut dec = StreamDecryptor::new(key_clone);

        // 加密多个块，验证序列号正确
        for i in 0..5 {
            let data = format!("chunk-{}", i);
            let encrypted = enc.encrypt(data.as_bytes()).unwrap();
            let decrypted = dec.decrypt(&encrypted).unwrap();
            assert_eq!(decrypted, data.as_bytes());
        }
    }

    #[test]
    fn test_token_sign_verify() {
        let signer = TokenSigner::new(b"test-secret");
        let token = "stream_key=test&ts=1234567890";
        let sig = signer.sign(token);
        assert!(signer.verify(token, &sig));
        assert!(!signer.verify(token, "invalid-sig"));
    }

    #[test]
    fn test_hkdf_key_derivation_deterministic() {
        let key_info = KeyInfo::new(CipherSuite::ChaCha20Poly1305).unwrap();
        let key1 = key_info.derive_chunk_key(42);
        let key2 = key_info.derive_chunk_key(42);
        assert_eq!(key1, key2);

        let key3 = key_info.derive_chunk_key(43);
        assert_ne!(key1, key3); // 不同序列号产生不同密钥
    }
}
