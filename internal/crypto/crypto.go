package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// CipherSuite defines supported encryption methods
type CipherSuite string

const (
	CipherChaCha20 CipherSuite = "chacha20-poly1305"
	CipherAES128   CipherSuite = "aes-128-cbc"
)

// NormalizeCipherSuite accepts historical short names and returns the canonical
// wire value used by Agents and persisted StreamInfo records.
func NormalizeCipherSuite(s string) (CipherSuite, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "chacha20", "chacha20-poly1305":
		return CipherChaCha20, nil
	case "aes", "aes128", "aes-128", "aes-128-cbc":
		return CipherAES128, nil
	default:
		return "", fmt.Errorf("unsupported cipher suite: %s", s)
	}
}

// KeyInfo holds encryption key material
type KeyInfo struct {
	Key       []byte
	IV        []byte
	Cipher    CipherSuite
	StreamKey string
}

// GenerateKey creates a new encryption key for a stream
func GenerateKey(suite CipherSuite) (*KeyInfo, error) {
	ki := &KeyInfo{Cipher: suite}
	switch suite {
	case CipherChaCha20:
		ki.Key = make([]byte, chacha20poly1305.KeySize)
		if _, err := io.ReadFull(rand.Reader, ki.Key); err != nil {
			return nil, err
		}
	case CipherAES128:
		ki.Key = make([]byte, 16) // AES-128
		ki.IV = make([]byte, 16)
		if _, err := io.ReadFull(rand.Reader, ki.Key); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(rand.Reader, ki.IV); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported cipher suite: %s", suite)
	}
	return ki, nil
}

// KeyToBase64 encodes key info to base64 strings for transport
func (ki *KeyInfo) KeyToBase64() (key, iv string) {
	key = base64.StdEncoding.EncodeToString(ki.Key)
	if ki.IV != nil {
		iv = base64.StdEncoding.EncodeToString(ki.IV)
	}
	return
}

// KeyFromBase64 decodes key info from base64 strings
func KeyFromBase64(keyStr, ivStr string, suite CipherSuite) (*KeyInfo, error) {
	ki := &KeyInfo{Cipher: suite}
	var err error
	ki.Key, err = base64.StdEncoding.DecodeString(keyStr)
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	if ivStr != "" {
		ki.IV, err = base64.StdEncoding.DecodeString(ivStr)
		if err != nil {
			return nil, fmt.Errorf("decode iv: %w", err)
		}
	}
	return ki, nil
}

// EncryptChunk encrypts a data chunk (ts segment or partial stream data)
func EncryptChunk(data []byte, ki *KeyInfo) ([]byte, error) {
	switch ki.Cipher {
	case CipherChaCha20:
		return encryptChaCha20(data, ki.Key)
	case CipherAES128:
		return encryptAES128CBC(data, ki.Key, ki.IV)
	default:
		return nil, fmt.Errorf("unsupported cipher: %s", ki.Cipher)
	}
}

// DecryptChunk decrypts a data chunk
func DecryptChunk(data []byte, ki *KeyInfo) ([]byte, error) {
	switch ki.Cipher {
	case CipherChaCha20:
		return decryptChaCha20(data, ki.Key)
	case CipherAES128:
		return decryptAES128CBC(data, ki.Key, ki.IV)
	default:
		return nil, fmt.Errorf("unsupported cipher: %s", ki.Cipher)
	}
}

// --- ChaCha20-Poly1305 ---

func encryptChaCha20(plaintext, key []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	// Format: nonce || ciphertext || tag
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

func decryptChaCha20(ciphertext, key []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonceSize := aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return aead.Open(nil, nonce, ciphertext, nil)
}

// --- AES-128-CBC (HLS EXT-X-KEY compatible) ---

func encryptAES128CBC(plaintext, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	// PKCS7 padding
	padLen := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	padded := make([]byte, len(plaintext)+padLen)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	ciphertext := make([]byte, len(padded))
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(ciphertext, padded)
	return ciphertext, nil
}

func decryptAES128CBC(ciphertext, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("ciphertext is not a multiple of block size")
	}
	plaintext := make([]byte, len(ciphertext))
	mode := cipher.NewCBCDecrypter(block, iv)
	mode.CryptBlocks(plaintext, ciphertext)
	// Remove PKCS7 padding
	if len(plaintext) == 0 {
		return plaintext, nil
	}
	padLen := int(plaintext[len(plaintext)-1])
	if padLen > aes.BlockSize || padLen == 0 {
		return nil, errors.New("invalid padding")
	}
	return plaintext[:len(plaintext)-padLen], nil
}

// --- Stream Encryptor for continuous data ---

// StreamEncryptor handles continuous stream encryption with sequence numbers
type StreamEncryptor struct {
	keyInfo *KeyInfo
	seqNum  uint32
}

func NewStreamEncryptor(ki *KeyInfo) *StreamEncryptor {
	return &StreamEncryptor{keyInfo: ki}
}

// Encrypt encrypts a stream chunk with a derived per-chunk key
func (se *StreamEncryptor) Encrypt(data []byte) ([]byte, error) {
	// Derive per-chunk IV/key using sequence number for forward secrecy
	seqBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(seqBytes, se.seqNum)
	se.seqNum++

	chunkKey := deriveChunkKey(se.keyInfo.Key, seqBytes)
	chunkIV := deriveChunkIV(se.keyInfo.IV, seqBytes)

	switch se.keyInfo.Cipher {
	case CipherChaCha20:
		return encryptChaCha20(data, chunkKey)
	case CipherAES128:
		return encryptAES128CBC(data, chunkKey, chunkIV)
	default:
		return nil, fmt.Errorf("unsupported cipher: %s", se.keyInfo.Cipher)
	}
}

// StreamDecryptor handles continuous stream decryption
type StreamDecryptor struct {
	keyInfo *KeyInfo
	seqNum  uint32
}

func NewStreamDecryptor(ki *KeyInfo) *StreamDecryptor {
	return &StreamDecryptor{keyInfo: ki}
}

// Decrypt decrypts a stream chunk
func (sd *StreamDecryptor) Decrypt(data []byte) ([]byte, error) {
	seqBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(seqBytes, sd.seqNum)
	sd.seqNum++

	chunkKey := deriveChunkKey(sd.keyInfo.Key, seqBytes)
	chunkIV := deriveChunkIV(sd.keyInfo.IV, seqBytes)

	switch sd.keyInfo.Cipher {
	case CipherChaCha20:
		return decryptChaCha20(data, chunkKey)
	case CipherAES128:
		return decryptAES128CBC(data, chunkKey, chunkIV)
	default:
		return nil, fmt.Errorf("unsupported cipher: %s", sd.keyInfo.Cipher)
	}
}

// deriveChunkKey generates a per-chunk encryption key from master key + sequence
func deriveChunkKey(masterKey []byte, seq []byte) []byte {
	h := sha256.New()
	h.Write(masterKey)
	h.Write(seq)
	derived := h.Sum(nil)
	if len(masterKey) <= 32 {
		return derived[:len(masterKey)]
	}
	return derived
}

// deriveChunkIV generates a per-chunk IV from base IV + sequence
func deriveChunkIV(baseIV []byte, seq []byte) []byte {
	if baseIV == nil {
		return nil
	}
	h := sha256.New()
	h.Write(baseIV)
	h.Write(seq)
	derived := h.Sum(nil)
	return derived[:len(baseIV)]
}

// TokenSigner handles HMAC-SHA256 token signing and verification
type TokenSigner struct {
	key []byte
}

func NewTokenSigner(secret []byte) *TokenSigner {
	h := sha256.New()
	h.Write(secret)
	key := h.Sum(nil)
	return &TokenSigner{key: key}
}

func (ts *TokenSigner) Sign(data string) string {
	h := hmac.New(sha256.New, ts.key)
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func (ts *TokenSigner) Verify(data string, signature string) bool {
	expected := ts.Sign(data)
	return hmac.Equal([]byte(expected), []byte(signature))
}
