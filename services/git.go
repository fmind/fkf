package services

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/fmind/fkf/core"
)

// runGit invokes fkf's fixed Git operations from the host PATH, never from mutable content
// below the base. Provider commands use the separately trusted base.Env path instead.
func runGit(ctx context.Context, root string, timeout time.Duration, args ...string) (string, error) {
	hostContext, err := core.WithCommandEnvironment(ctx, map[string]string{
		"PATH": core.SanitizePathList(os.Getenv("PATH"), root),
	})
	if err != nil {
		return "", fmt.Errorf("prepare host Git environment: %w", err)
	}
	command := append([]string{"git"}, args...)
	return core.RunCLI(hostContext, command, root, timeout)
}
