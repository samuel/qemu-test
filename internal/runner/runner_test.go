package runner

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveKVM(t *testing.T) {
	if got, err := ResolveKVM("off", runtime.GOARCH); err != nil || got {
		t.Errorf("ResolveKVM(off) = %v, %v; want false, nil", got, err)
	}
	if got, err := ResolveKVM("on", runtime.GOARCH); err != nil || !got {
		t.Errorf("ResolveKVM(on) = %v, %v; want true, nil", got, err)
	}
	if _, err := ResolveKVM("bogus", runtime.GOARCH); err == nil {
		t.Error("ResolveKVM(bogus) = nil error, want error")
	}

	// auto never enables for a guest arch that cannot match the host, whatever
	// /dev/kvm says.
	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}
	if got, err := ResolveKVM("auto", otherArch); err != nil || got {
		t.Errorf("ResolveKVM(auto, cross-arch %q) = %v, %v; want false, nil", otherArch, got, err)
	}

	// auto on the host arch must not error (result depends on /dev/kvm presence).
	if _, err := ResolveKVM("auto", runtime.GOARCH); err != nil {
		t.Errorf("ResolveKVM(auto, host arch) error: %v", err)
	}
}

func TestTailSuffix(t *testing.T) {
	if got := tailSuffix(nil); got != "" {
		t.Errorf("tailSuffix(nil) = %q, want empty", got)
	}
	got := tailSuffix([]string{"line one", "line two"})
	want := "; last console output:\nline one\nline two"
	if got != want {
		t.Errorf("tailSuffix = %q, want %q", got, want)
	}
}

func TestFirmwareDirs(t *testing.T) {
	// The first candidate derives from the binary: bin/X -> share/qemu.
	got := firmwareDirs("/opt/homebrew/bin/qemu-system-loongarch64")
	if len(got) == 0 || got[0] != "/opt/homebrew/share/qemu" {
		t.Errorf("firmwareDirs[0] = %q, want /opt/homebrew/share/qemu", got[0])
	}
	if d := firmwareDirs("/usr/bin/qemu-system-loongarch64"); d[0] != "/usr/share/qemu" {
		t.Errorf("firmwareDirs[0] = %q, want /usr/share/qemu", d[0])
	}
	// An empty binary path contributes no derived dir, only the fixed fallbacks.
	if d := firmwareDirs(""); len(d) == 0 || d[0] == "" || strings.HasPrefix(d[0], "..") {
		t.Errorf("firmwareDirs(\"\") = %v, want only fixed dirs", d)
	}
}

func TestFindFirmwareIn(t *testing.T) {
	dir := t.TempDir()
	const name = "edk2-loongarch64-code.fd"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("fw"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory named like the firmware must not be mistaken for the file.
	missesDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(missesDir, name), 0o755); err != nil {
		t.Fatal(err)
	}

	if p, ok := findFirmwareIn([]string{missesDir, dir}, name); !ok || p != filepath.Join(dir, name) {
		t.Errorf("findFirmwareIn = %q, %v; want the real file in %q", p, ok, dir)
	}
	if _, ok := findFirmwareIn([]string{missesDir, t.TempDir()}, name); ok {
		t.Error("findFirmwareIn matched a directory or missing file")
	}
}

func TestQuoteCommand(t *testing.T) {
	got := quoteCommand("qemu-system-x86_64", []string{"-append", "console=ttyS0 quiet", "-smp", "2"})
	want := "qemu-system-x86_64 -append 'console=ttyS0 quiet' -smp 2"
	if got != want {
		t.Errorf("quoteCommand = %q, want %q", got, want)
	}
	if strings.Contains(quoteCommand("q", []string{"-smp", "2"}), "'") {
		t.Error("quoteCommand should not quote args without whitespace")
	}
}
