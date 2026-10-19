package domain

import (
	"net/mail"
	"strings"
)

const MaxEmailLength = 254

// Email là value object: một giá trị Email chỉ có thể được tạo ra qua ParseEmail (đã chuẩn
// hoá và hợp lệ) hoặc EmailFromStore (dữ liệu đã được validate khi ghi). Code nhận một
// Email không cần kiểm tra lại, và không thể vô tình gán một chuỗi rác vào User.Email.
type Email struct{ v string }

// ParseEmail chuẩn hoá (trim, chữ thường) rồi kiểm tra địa chỉ email.
// Lỗi trả về là *ValidationError với field "email".
func ParseEmail(raw string) (Email, error) {
	e, msg := parseEmail(raw)
	if msg != "" {
		v := &ValidationError{}
		v.Add("email", msg)
		return Email{}, v
	}
	return e, nil
}

// EmailFromStore dựng Email từ dữ liệu đã lưu mà KHÔNG validate lại. Cố ý như vậy: nếu sau này
// quy tắc validate được thắt chặt, các bản ghi cũ vẫn phải đọc được, nếu không một thay đổi
// quy tắc sẽ làm mọi request đọc bản ghi cũ bị lỗi 500. Chỉ adapter lưu trữ được dùng hàm này.
func EmailFromStore(s string) Email { return Email{v: s} }

func (e Email) String() string { return e.v }

func (e Email) IsZero() bool { return e.v == "" }

// parseEmail trả thông báo lỗi rỗng khi hợp lệ.
func parseEmail(raw string) (Email, string) {
	email := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case email == "":
		return Email{}, "is required"
	case len(email) > MaxEmailLength:
		return Email{}, "must be at most 254 characters"
	}
	addr, err := mail.ParseAddress(email)
	// Chỉ chấp nhận địa chỉ trần ("a@b.com"), không chấp nhận "Tên <a@b.com>".
	if err != nil || addr.Address != email || addr.Name != "" {
		return Email{}, "must be a valid email address"
	}
	return Email{v: email}, ""
}
