package memory_test

import (
	"testing"

	"github.com/example/userapi/internal/adapter/outbound/memory"
	"github.com/example/userapi/internal/adapter/outbound/repotest"
	"github.com/example/userapi/internal/core/port"
)

func TestUserRepositoryContract(t *testing.T) {
	repotest.Run(t, func(t *testing.T) port.UserRepository {
		return memory.NewUserRepository()
	})
}
