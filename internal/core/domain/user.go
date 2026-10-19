// Package domain chứa entity, value object và lỗi nghiệp vụ.
// Package này KHÔNG import bất kỳ framework, driver hay adapter nào:
// nó là trung tâm của kiến trúc Hexagonal.
package domain

import (
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxNameLength  = 100
	MaxEmailLength = 254
)

// UserID là định danh của User, dạng UUID chuẩn (36 ký tự, chữ thường).
type UserID string

// Valid kiểm tra UserID có đúng dạng UUID chuẩn hay không.
// Adapter dùng nó để từ chối sớm ID rác thay vì gửi xuống database.
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
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
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
	Email     string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewUser tạo User mới sau khi chuẩn hoá và kiểm tra dữ liệu đầu vào.
func NewUser(id UserID, email, name string, now time.Time) (User, error) {
	v := &ValidationError{}
	normEmail := normalizeEmail(email, v)
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
	}, nil
}

// Changes mô tả một cập nhật từng phần (PATCH): field nil nghĩa là giữ nguyên.
type Changes struct {
	Email *string
	Name  *string
}

// Apply áp dụng thay đổi lên bản sao của User và trả về bản mới.
// Nếu không có gì thay đổi thì UpdatedAt giữ nguyên.
func (u User) Apply(c Changes, now time.Time) (User, error) {
	v := &ValidationError{}
	next := u
	if c.Email != nil {
		next.Email = normalizeEmail(*c.Email, v)
	}
	if c.Name != nil {
		next.Name = normalizeName(*c.Name, v)
	}
	if v.HasErrors() {
		return User{}, v
	}
	if next.Email != u.Email || next.Name != u.Name {
		next.UpdatedAt = now
	}
	return next, nil
}

func normalizeEmail(raw string, v *ValidationError) string {
	email := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case email == "":
		v.Add("email", "is required")
	case len(email) > MaxEmailLength:
		v.Add("email", "must be at most 254 characters")
	default:
		addr, err := mail.ParseAddress(email)
		// Chỉ chấp nhận địa chỉ trần ("a@b.com"), không chấp nhận "Tên <a@b.com>".
		if err != nil || addr.Address != email || addr.Name != "" {
			v.Add("email", "must be a valid email address")
		}
	}
	return email
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
