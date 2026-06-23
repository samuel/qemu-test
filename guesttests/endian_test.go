package guesttests

import (
	"encoding/binary"
	"runtime"
	"testing"
)

// bigEndianArches are the Go GOARCH values whose native byte order is big-endian.
var bigEndianArches = map[string]bool{
	"s390x":  true,
	"ppc64":  true,
	"mips":   true,
	"mips64": true,
}

// TestNativeByteOrder asserts the running binary's native byte order matches its
// build GOARCH. Under `qemu-test -arch s390x` / `-arch ppc64` the guest binary is
// big-endian, so this exercises and proves the big-endian path end-to-end; on a
// little-endian host (`go test ./...`, `-arch amd64`) it confirms little-endian.
func TestNativeByteOrder(t *testing.T) {
	var buf [2]byte
	binary.NativeEndian.PutUint16(buf[:], 0x0102)
	gotBigEndian := buf[0] == 0x01 // most-significant byte first => big-endian
	wantBigEndian := bigEndianArches[runtime.GOARCH]
	if gotBigEndian != wantBigEndian {
		t.Fatalf("GOARCH %q: native byte order big-endian=%v (bytes %#x), want big-endian=%v",
			runtime.GOARCH, gotBigEndian, buf, wantBigEndian)
	}
	t.Logf("GOARCH %q native byte order: big-endian=%v", runtime.GOARCH, gotBigEndian)
}
