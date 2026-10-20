// Command userapi is the composition root entry point: read config, create a logger, handle signals,
// and delegate to internal/app, which wires adapters into core and manages the server lifecycle.
//
// Usage:
//
//	userapi           run the HTTP API
//	userapi migrate   apply PostgreSQL migrations and exit (Job / initContainer)
//	userapi relay     forward outbox events to the broker (EVENTS_SINK), as a separate Deployment
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

// version is set at build time: go build -ldflags "-X main.version=$(git describe --tags)".
var version = "dev"

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	// ContextHandler adds request_id, trace_id, and span_id from ctx to every *Context(ctx, ...) log.
	logger := slog.New(observability.NewContextHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}),
	))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Unregister after the first signal: the second signal (another Ctrl+C) terminates immediately.
	context.AfterFunc(ctx, stop)

	switch cmd := strings.Join(os.Args[1:], " "); cmd {
	case "":
		err = app.Run(ctx, cfg, logger, app.Options{Version: version})
	case "migrate":
		err = app.Migrate(ctx, cfg, logger)
	case "relay":
		err = app.RunRelay(ctx, cfg, logger, app.Options{Version: version})
	default:
		err = fmt.Errorf("unknown command %q (usage: userapi [migrate|relay])", cmd)
	}
	if err != nil {
		logger.Error("userapi stopped with error", "error", err)
		os.Exit(1)
	}
}
