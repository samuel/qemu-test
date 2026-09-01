package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestWaitStatusErr(t *testing.T) {
	// WaitStatus layout: low 7 bits are the signal (0 => exited), bits 8..15 the
	// exit code.
	tests := []struct {
		name   string
		ws     unix.WaitStatus
		wantOK bool   // expect nil error
		want   string // else expected error text
	}{
		{name: "exit 0", ws: unix.WaitStatus(0), wantOK: true},
		{name: "exit 1", ws: unix.WaitStatus(1 << 8), want: "exit status 1"},
		{name: "exit 2", ws: unix.WaitStatus(2 << 8), want: "exit status 2"},
		{name: "killed", ws: unix.WaitStatus(unix.SIGKILL), want: "signal: killed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := waitStatusErr(tt.ws)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("waitStatusErr(%#x) = %v, want nil", uint32(tt.ws), err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("waitStatusErr(%#x) = %v, want %q", uint32(tt.ws), err, tt.want)
			}
		})
	}
}
