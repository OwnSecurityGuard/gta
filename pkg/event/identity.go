package event

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

// EventID 是事件的全局唯一标识符
// 使用 UUIDv7 格式，保证时间有序且全局唯一
type EventID string

// EventType 是事件类型的字符串表示
// 例如："network.packet", "http.request", "game.login"
type EventType string

// SourceID 是事件的创建者/来源
// 例如："tcp-capture", "http-decoder", "ai-analyzer"
type SourceID string

// Identity 描述事件的身份信息
// 回答"这是什么事件？"的问题
type Identity struct {
	// ID 是事件的全局唯一标识符（UUIDv7）
	ID EventID

	// SessionID 是捕获或分析会话的标识
	SessionID string

	// Type 是事件类型（如 "network.packet", "http.request"）
	Type EventType

	// SchemaID 是载荷的 schema 版本标识（如 "http.request.v1"）
	SchemaID string

	// Source 是事件的创建者/来源
	Source SourceID

	// Timestamp 是事件发生的时间
	Timestamp time.Time
}

// 错误定义
var (
	ErrMissingEventID   = errors.New("missing event ID")
	ErrMissingEventType = errors.New("missing event type")
	ErrMissingTimestamp = errors.New("missing timestamp")
)

// uuidv7Mutex 保护 lastTimestamp 和 sequence 的并发访问
var (
	uuidv7Mutex    sync.Mutex
	uuidv7LastMs   int64
	uuidv7Sequence uint16
)

// NewEventID 生成新的 UUIDv7 事件 ID
// UUIDv7 基于时间戳，保证时间有序且全局唯一
// 格式：48bit timestamp(ms) + 4bit version(7) + 12bit counter + 2bit variant(10) + 62bit rand
// 同一毫秒内使用 12-bit 单调计数器，避免冲突并保持时间排序。
func NewEventID() EventID {
	uuidv7Mutex.Lock()
	defer uuidv7Mutex.Unlock()

	nowMs := time.Now().UnixMilli()

	if nowMs <= uuidv7LastMs {
		// 同一毫秒或时钟回拨：递增 12-bit 计数器
		uuidv7Sequence++
		if uuidv7Sequence > 0xFFF {
			// 计数器溢出：将时间戳推进 1ms 并重置计数器，保持单调递增
			uuidv7LastMs++
			nowMs = uuidv7LastMs
			uuidv7Sequence = 0
		}
	} else {
		// 新毫秒：随机初始化 12-bit 计数器
		uuidv7LastMs = nowMs
		var b [2]byte
		_, _ = rand.Read(b[:])
		uuidv7Sequence = (uint16(b[0])<<4 | uint16(b[1])>>4) & 0x0FFF
	}

	var uuid [16]byte

	// 填充 48 位时间戳（毫秒）
	binary.BigEndian.PutUint32(uuid[0:4], uint32(nowMs>>16))
	binary.BigEndian.PutUint16(uuid[4:6], uint16(nowMs))

	// 填充 12-bit 计数器到 rand_a，并设置版本为 7（byte 6 高 4 位）
	uuid[6] = 0x70 | byte(uuidv7Sequence>>8)&0x0F
	uuid[7] = byte(uuidv7Sequence)

	// 填充 62 位随机数到 rand_b，并设置变体为 10xx（byte 8 高 2 位）
	_, _ = rand.Read(uuid[8:])
	uuid[8] = (uuid[8] & 0x3F) | 0x80

	return EventID(formatUUID(uuid))
}

// formatUUID 将 16 字节格式化为标准 UUID 字符串
func formatUUID(uuid [16]byte) string {
	const hex = "0123456789abcdef"
	buf := make([]byte, 36)

	pos := 0
	for i := 0; i < 16; i++ {
		// 在字节 4, 6, 8, 10 之前插入连字符
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[pos] = '-'
			pos++
		}
		buf[pos] = hex[uuid[i]>>4]
		buf[pos+1] = hex[uuid[i]&0x0F]
		pos += 2
	}
	return string(buf)
}

// NewIdentity 创建新的 Identity
// 自动生成 UUIDv7 作为 ID，使用当前时间作为 Timestamp
func NewIdentity(sessionID string, eventType EventType, schemaID string, source SourceID) Identity {
	return Identity{
		ID:        NewEventID(),
		SessionID: sessionID,
		Type:      eventType,
		SchemaID:  schemaID,
		Source:    source,
		Timestamp: time.Now(),
	}
}

// NewIdentityWithTime 创建新的 Identity，使用指定的时间戳
func NewIdentityWithTime(sessionID string, eventType EventType, schemaID string, source SourceID, timestamp time.Time) Identity {
	return Identity{
		ID:        NewEventID(),
		SessionID: sessionID,
		Type:      eventType,
		SchemaID:  schemaID,
		Source:    source,
		Timestamp: timestamp,
	}
}

// IsZero 检查 Identity 是否为零值
func (i Identity) IsZero() bool {
	return i.ID == "" && i.SessionID == "" && i.Type == ""
}

// Validate 验证 Identity 的必填字段
func (i Identity) Validate() error {
	if i.ID == "" {
		return ErrMissingEventID
	}
	if i.Type == "" {
		return ErrMissingEventType
	}
	if i.Timestamp.IsZero() {
		return ErrMissingTimestamp
	}
	return nil
}

// String 返回 EventID 的字符串表示
func (id EventID) String() string {
	return string(id)
}

// IsZero 检查 EventID 是否为空
func (id EventID) IsZero() bool {
	return id == ""
}

// String 返回 EventType 的字符串表示
func (t EventType) String() string {
	return string(t)
}

// String 返回 SourceID 的字符串表示
func (s SourceID) String() string {
	return string(s)
}

// MustNewEventID 生成新的 EventID，失败时 panic
func MustNewEventID() EventID {
	id := NewEventID()
	if id == "" {
		panic(fmt.Errorf("failed to generate event ID"))
	}
	return id
}
