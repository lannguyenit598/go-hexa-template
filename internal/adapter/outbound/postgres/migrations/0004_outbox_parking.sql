-- +goose Up
-- Message bị broker từ chối vĩnh viễn (ví dụ quá lớn) được tách khỏi hàng đợi thay vì chặn mọi
-- message phía sau: relay ghi parked_at và lý do, rồi không lấy lại nó nữa. Chỉ thêm cột
-- nullable, nên an toàn khi rolling deploy: relay cũ không biết tới hai cột này vẫn chạy được.
ALTER TABLE outbox
    ADD COLUMN IF NOT EXISTS parked_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_error TEXT;

-- +goose Down
ALTER TABLE outbox
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS parked_at;
