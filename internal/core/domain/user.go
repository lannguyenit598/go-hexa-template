// Package domain chứa entity, value object và lỗi nghiệp vụ.
// Package này KHÔNG import bất kỳ framework, driver hay adapter nào:
// nó là trung tâm của kiến trúc Hexagonal.
package domain

import (
	"strings"
	"time"
	"unicode/utf8"
)

const MaxNameLength = 100

// UserID là định danh của User, dạng UUID chuẩn (36 ký tự, chữ thường).
type UserID string

// Valid kiểm tra UserID có đúng dạng UUID chuẩn hay không.
// Service dùng nó để từ chối sớm ID rác thay vì gửi xuống database.
func (id UserID) Valid() bool {
	s := string(id)
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

func (id UserID) String() string { return string(id) }

// User là aggregate root. Mọi thay đổi trạng thái đi qua method để giữ invariant.
type User struct {
	ID        UserID
	Email     Email
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Version tăng 1 sau mỗi thay đổi; dùng cho optimistic locking và ETag.
	Version int64
}

// NewUser tạo User mới sau khi chuẩn hoá và kiểm tra dữ liệu đầu vào.
func NewUser(id UserID, email, name string, now time.Time) (User, error) {
	v := &ValidationError{}
	normEmail := collectEmail(email, v)
	normName := normalizeName(name, v)
	if v.HasErrors() {
		return User{}, v
	}
	return User{
		ID:        id,
		Email:     normEmail,
		Name:      normName,
		CreatedAt: now,
		UpdatedAt: now,
		Version:   1,
	}, nil
}

// Changes mô tả một cập nhật từng phần (PATCH): field nil nghĩa là giữ nguyên.
type Changes struct {
	Email *string
	Name  *string
}

// Apply áp dụng thay đổi lên bản sao của User và trả về bản mới.
// Nếu không có gì thay đổi thì UpdatedAt và Version giữ nguyên.
func (u User) Apply(c Changes, now time.Time) (User, error) {
	v := &ValidationError{}
	next := u
	if c.Email != nil {
		next.Email = collectEmail(*c.Email, v)
	}
	if c.Name != nil {
		next.Name = normalizeName(*c.Name, v)
	}
	if v.HasErrors() {
		return User{}, v
	}
	if next.Email != u.Email || next.Name != u.Name {
		next.UpdatedAt = now
		next.Version = u.Version + 1
	}
	return next, nil
}

// collectEmail parse email và gom lỗi (nếu có) vào v, để báo mọi field sai cùng lúc.
func collectEmail(raw string, v *ValidationError) Email {
	e, msg := parseEmail(raw)
	if msg != "" {
		v.Add("email", msg)
	}
	return e
}

func normalizeName(raw string, v *ValidationError) string {
	name := strings.Join(strings.Fields(raw), " ") // gộp khoảng trắng thừa
	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		v.Add("name", "is required")
	case n > MaxNameLength:
		v.Add("name", "must be at most 100 characters")
	}
	return name
}
