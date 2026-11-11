//go:build !linux

package main

import "log/slog"

func mountfs() error {
	slog.Error("mountfs not implemented")
	return nil
}

func mountShares() error {
	slog.Error("mountShares not implemented")
	return nil
}
