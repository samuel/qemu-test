package guesttests

import (
	"os"
	"testing"
)

// TestEnv passes by default and fails when GUESTTEST_FAIL is set, exercising
// -env passing and the failure path:
//
//	qemu-test -env GUESTTEST_FAIL=1 ./internal/guesttests
func TestEnv(t *testing.T) {
	if v := os.Getenv("GUESTTEST_FAIL"); v != "" {
		t.Fatalf("failing because GUESTTEST_FAIL=%q is set", v)
	}
	t.Log("succeeded; set GUESTTEST_FAIL=1 to force a failure")
}
