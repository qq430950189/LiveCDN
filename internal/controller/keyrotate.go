package controller

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/user/live-cdn/internal/common"
	"github.com/user/live-cdn/internal/crypto"
)

const keyRotationInterval = 10 * time.Minute

// KeyRotator 管理直播流的密钥轮换
type KeyRotator struct {
	store    *MemoryStore
	mu       sync.Mutex
	stopCh   chan struct{}
}

func NewKeyRotator(store *MemoryStore) *KeyRotator {
	return &KeyRotator{
		store:  store,
		stopCh: make(chan struct{}),
	}
}

// Start 启动定期密钥轮换
func (kr *KeyRotator) Start() {
	go kr.rotateLoop()
}

// Stop 停止密钥轮换
func (kr *KeyRotator) Stop() {
	close(kr.stopCh)
}

func (kr *KeyRotator) rotateLoop() {
	ticker := time.NewTicker(keyRotationInterval)
	defer ticker.Stop()

	for {
		select {
		case <-kr.stopCh:
			return
		case <-ticker.C:
			kr.RotateAllLiveStreams()
		}
	}
}

func (kr *KeyRotator) RotateAllLiveStreams() {
	kr.mu.Lock()
	defer kr.mu.Unlock()

	streams := kr.store.GetLiveStreams()
	rotated := 0

	for _, stream := range streams {
		if time.Since(stream.CreatedAt) < keyRotationInterval {
			continue // 刚创建的流不需要轮换
		}

		// 生成新密钥
		suite := crypto.CipherSuite(stream.CipherSuite)
		if suite == "" {
			suite = crypto.CipherChaCha20
		}

		keyInfo, err := crypto.GenerateKey(suite)
		if err != nil {
			log.Error().Err(err).Str("stream", stream.StreamKey[:8]).Msg("key rotation failed")
			continue
		}

		newKey, newIV := keyInfo.KeyToBase64()

		// 原子更新密钥
		updated := &common.StreamInfo{
			StreamKey:   stream.StreamKey,
			Title:       stream.Title,
			OriginURL:   stream.OriginURL,
			EncryptKey:  newKey,
			EncryptIV:   newIV,
			CipherSuite: stream.CipherSuite,
			IsLive:      true,
			CreatedAt:   stream.CreatedAt, // 保持原始创建时间
		}
		kr.store.CreateStream(updated) // 覆盖写入

		rotated++
		log.Info().
			Str("stream", stream.StreamKey[:8]).
			Str("cipher", stream.CipherSuite).
			Msg("key rotated")
	}

	if rotated > 0 {
		log.Info().Int("count", rotated).Msg("key rotation completed")
	}
}
