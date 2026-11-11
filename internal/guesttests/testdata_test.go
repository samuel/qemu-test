package guesttests

import (
	"os"
	"runtime"
	"testing"
)

// TestTestdata reads a file relative to the package directory, which works in
// the guest only because qemu-test shares the working directory at the same
// path and runs the test binary there.
func TestTestdata(t *testing.T) {
	const want = "hello from the host\n"
	got, err := os.ReadFile("testdata/hello.txt")
	if err != nil {
		switch runtime.GOARCH {
		case "arm", "riscv64":
			// Their default bundles carry no 9p driver, so the share is empty.
			t.Skipf("working directory not shared on %s: %v", runtime.GOARCH, err)
		}
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("testdata/hello.txt = %q, want %q", got, want)
	}
}
