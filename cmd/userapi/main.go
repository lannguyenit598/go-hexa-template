// Command userapi là điểm vào của composition root: đọc config, tạo logger, bắt tín hiệu rồi
// giao cho internal/app (nơi lắp adapter vào core và quản lý vòng đời server).
//
// Cách dùng:
//
//	userapi           chạy HTTP API
//	userapi migrate   áp dụng migration PostgreSQL rồi thoát (dùng cho Job / initContainer)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/example/userapi/internal/app"
	"github.com/example/userapi/internal/config"
	"github.com/example/userapi/internal/observability"
)

// version được gán lúc build: go build -ldflags "-X main.version=$(git describe --tags)".
var version = "dev"

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	// ContextHandler thêm request_id, trace_id, span_id từ ctx vào mọi dòng log *Context(ctx, ...).
	logger := slog.New(observability.NewContextHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}),
	))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Sau tín hiệu đầu tiên thì bỏ đăng ký: tín hiệu thứ hai (Ctrl+C lần nữa) kết thúc tiến trình ngay.
	context.AfterFunc(ctx, stop)

	switch cmd := strings.Join(os.Args[1:], " "); cmd {
	case "":
		err = app.Run(ctx, cfg, logger, app.Options{Version: version})
	case "migrate":
		err = app.Migrate(ctx, cfg, logger)
	default:
		err = fmt.Errorf("unknown command %q (usage: userapi [migrate])", cmd)
	}
	if err != nil {
		logger.Error("userapi stopped with error", "error", err)
		os.Exit(1)
	}
}
