package domain

import (
	"errors"
	"sort"
	"strings"
)

// Lỗi nghiệp vụ dạng sentinel. Adapter inbound (HTTP, gRPC, CLI...) tự quyết định
// ánh xạ chúng sang mã lỗi của giao thức; core không biết gì về HTTP status.
var (
	ErrUserNotFound       = errors.New("user not found")
	ErrEmailAlreadyExists = errors.New("email already exists")
	// ErrVersionConflict: bản ghi đã bị thay đổi kể từ lúc được đọc (optimistic locking).
	ErrVersionConflict = errors.New("version conflict")
)

// FieldError mô tả một lỗi trên một trường dữ liệu.
type FieldError struct {
	Field   string
	Message string
}

// ValidationError gom tất cả lỗi kiểm tra dữ liệu để trả về một lần.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Add(field, message string) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: message})
}

func (e *ValidationError) HasErrors() bool { return len(e.Fields) > 0 }

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+" "+f.Message)
	}
	sort.Strings(parts)
	return "validation failed: " + strings.Join(parts, "; ")
}
