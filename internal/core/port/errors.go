package port

import "errors"

// Lỗi kết quả của use case, dùng chung cho mọi aggregate. Chúng thuộc tầng application chứ
// không thuộc domain: "không tìm thấy" hay "bị sửa đồng thời" là kết quả của việc đọc/ghi,
// không phải quy tắc nghiệp vụ. Ngữ cảnh được thêm bằng wrap, ví dụ
// fmt.Errorf("user %s: %w", id, port.ErrNotFound); so sánh bằng errors.Is.
// Repository trả sentinel trần; service thêm ngữ cảnh.
var (
	ErrNotFound = errors.New("not found")
	// ErrVersionConflict: bản ghi đã bị thay đổi kể từ lúc được đọc (optimistic locking).
	ErrVersionConflict = errors.New("version conflict")
)
