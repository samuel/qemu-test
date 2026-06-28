//go:build !linux

package main

import "log/slog"

func shutdown() {
	slog.Error("shutdown not implemented")
}
