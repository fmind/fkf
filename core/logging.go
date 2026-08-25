package core

import (
	"log/slog"
	"os"
)

// ConfigureLogging installs a text slog handler on stderr at info. There is no configurable
// level: fkf's own logging is one line per MCP call and a diagnostic when a declared command
// fails, and a level key would be one more thing in a base's configuration that is about fkf
// rather than about the base.
func ConfigureLogging() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
}
