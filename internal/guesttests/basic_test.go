package guesttests

import (
	"os"
	"testing"
)

// TestEnv passes by default so `go test ./...` and `qemu-test ./interal/guesttests` are
// green out of the box. Setting GUESTTEST_FAIL makes it fail, which exercises
// qemu-test's -env passing and its failure/exit-1 path:
//
//	qemu-test -env GUESTTEST_FAIL=1 ./interal/guesttests
func TestEnv(t *testing.T) {
	if v := os.Getenv("GUESTTEST_FAIL"); v != "" {
		t.Fatalf("failing because GUESTTEST_FAIL=%q is set", v)
	}
	t.Log("succeeded; set GUESTTEST_FAIL=1 to force a failure")
}
