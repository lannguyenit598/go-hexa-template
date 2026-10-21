package app

import (
	"context"
	"log/slog"
	"testing"

	"go.uber.org/fx"

	"github.com/example/userapi/internal/config"
)

// Wiring is resolved at runtime, so a missing provider must fail here rather than at startup.
func TestGraphs_ResolveEveryDependency(t *testing.T) {
	memory := config.Config{Storage: config.StorageMemory}
	pg := config.Config{Storage: config.StoragePostgres}
	pg.Events.Sink = config.EventsSinkKafka
	for name, tc := range map[string]struct {
		cfg   config.Config
		graph func(config.Config) fx.Option
	}{
		"api/memory":   {memory, apiGraph},
		"api/postgres": {pg, apiGraph},
		"relay/kafka":  {pg, relayGraph},
	} {
		t.Run(name, func(t *testing.T) {
			opts := appOptions(context.Background(), tc.cfg, slog.New(slog.DiscardHandler), Options{}, make(fatal, 1), tc.graph(tc.cfg))
			if err := fx.ValidateApp(opts); err != nil {
				t.Fatal(err)
			}
		})
	}
}
