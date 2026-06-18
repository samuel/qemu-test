//go:build !linux

package main

import "log/slog"

func mountfs() error {
	slog.Error("mountfs not implemented")
	return nil
}
