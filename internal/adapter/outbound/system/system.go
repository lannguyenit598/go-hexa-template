// Package system chứa adapter cho các phụ thuộc "hệ thống": đồng hồ và bộ sinh ID.
// Tách chúng thành port giúp test của core hoàn toàn tất định (deterministic).
package system

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
)

// Clock trả về giờ UTC, làm tròn xuống micro giây để khớp độ chính xác của PostgreSQL.
type Clock struct{}

var _ port.Clock = Clock{}

func (Clock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// UUIDv7 sinh UUID phiên bản 7 (RFC 9562): 48 bit timestamp mili giây + ngẫu nhiên.
// ID tăng dần theo thời gian nên index B-tree trong database ít bị phân mảnh.
type UUIDv7 struct {
	mu     sync.Mutex
	lastMs int64
	seq    uint16 // 12 bit rand_a, dùng làm bộ đếm khi nhiều ID trong cùng 1 ms
}

var _ port.IDGenerator = (*UUIDv7)(nil)

func NewUUIDv7() *UUIDv7 { return &UUIDv7{} }

func (g *UUIDv7) NewID() domain.UserID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand failed: " + err.Error()) // theo tài liệu Go, rand.Read không bao giờ lỗi
	}

	g.mu.Lock()
	ms := time.Now().UnixMilli()
	if ms > g.lastMs {
		g.lastMs = ms
		g.seq = uint16(b[6])<<8 | uint16(b[7])
		g.seq &= 0x07FF // để dư chỗ cho bộ đếm tăng
	} else {
		ms = g.lastMs // đồng hồ đứng yên hoặc lùi: giữ timestamp, tăng bộ đếm
		g.seq++
		if g.seq > 0x0FFF {
			g.lastMs++
			ms = g.lastMs
			g.seq = 0
		}
	}
	seq := g.seq
	g.mu.Unlock()

	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = 0x70 | byte(seq>>8)&0x0F // version 7
	b[7] = byte(seq)
	b[8] = b[8]&0x3F | 0x80 // variant RFC 9562

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return domain.UserID(out[:])
}
