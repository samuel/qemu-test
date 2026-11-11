package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestExitCode(t *testing.T) {
	// WaitStatus layout: low 7 bits are the signal (0 => exited), bits 8..15 the
	// exit code.
	tests := []struct {
		name    string
		ws      unix.WaitStatus
		want    int
		wantErr string
	}{
		{name: "exit 0", ws: unix.WaitStatus(0)},
		{name: "exit 1", ws: unix.WaitStatus(1 << 8), want: 1},
		{name: "exit 2", ws: unix.WaitStatus(2 << 8), want: 2},
		{name: "killed", ws: unix.WaitStatus(unix.SIGKILL), wantErr: "signal: killed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, err := exitCode(tt.ws)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("exitCode(%#x) error = %v, want %q", uint32(tt.ws), err, tt.wantErr)
				}
				return
			}
			if err != nil || code != tt.want {
				t.Fatalf("exitCode(%#x) = %d, %v, want %d, nil", uint32(tt.ws), code, err, tt.want)
			}
		})
	}
}
