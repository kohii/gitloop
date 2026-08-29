//go:build !darwin

package daemon

import (
	"context"
	"log/slog"
)

// watchNetworkChanges has no implementation outside macOS, which is the only
// platform gitloop supports. It exists so the package still builds — and its
// tests still run — on a machine used for cross-checking.
func watchNetworkChanges(ctx context.Context, _ func(), logger *slog.Logger) error {
	logger.Debug("network change detection is not implemented on this platform")
	<-ctx.Done()
	return nil
}
