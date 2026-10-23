//go:build windows

package main

import (
	"context"
	"log/slog"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
)

// watchSignals：Windows 没有 SIGHUP，靠配置文件监视和管理 API 重载。
func watchSignals(context.Context, *app.App, *slog.Logger) {}
