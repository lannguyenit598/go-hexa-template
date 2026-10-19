// Package system chứa adapter cho các phụ thuộc "hệ thống": đồng hồ và bộ sinh ID.
// Tách chúng thành port giúp test của core hoàn toàn tất định (deterministic).
package system

import (
	"time"

	"github.com/google/uuid"

	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
)

// Clock trả về giờ UTC, làm tròn xuống micro giây để khớp độ chính xác của PostgreSQL.
type Clock struct{}

var _ port.Clock = Clock{}

func (Clock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// UUIDv7 sinh UUID phiên bản 7 (RFC 9562): 48 bit timestamp mili giây + ngẫu nhiên.
// ID tăng dần theo thời gian nên index B-tree trong database ít bị phân mảnh.
// Dùng google/uuid (đã kiểm chứng rộng rãi, đơn điệu tăng trong một tiến trình) thay vì
// tự hiện thực: code sinh ID là thứ không nên phải tự bảo trì.
type UUIDv7 struct{}

var _ port.IDGenerator = UUIDv7{}

func NewUUIDv7() UUIDv7 { return UUIDv7{} }

func (UUIDv7) NewID() domain.UserID {
	// uuid.NewV7 chỉ lỗi khi crypto/rand lỗi, điều mà Go đảm bảo không xảy ra.
	return domain.UserID(uuid.Must(uuid.NewV7()).String())
}
