//go:build !linux

package main

import "log/slog"

func setupLoopback() { slog.Error("setupLoopback not implemented") }
