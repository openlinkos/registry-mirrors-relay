//go:build !windows

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
)

// watchSignals：收到 SIGHUP 时重新加载配置（systemctl reload、kill -HUP）。
func watchSignals(ctx context.Context, a *app.App, log *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				log.Info("收到 SIGHUP，重新加载配置")
				_ = a.ReloadConfig()
			}
		}
	}()
}
