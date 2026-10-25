//go:build windows

package main

import (
	"context"
	"log/slog"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
)

// watchSignals does nothing: Windows has no SIGHUP, so reloads rely on watching the configuration file
// and on the management API.
func watchSignals(context.Context, *app.App, *slog.Logger) {}
