package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestResolveKVM(t *testing.T) {
	if got, err := resolveKVM("off", runtime.GOARCH); err != nil || got {
		t.Errorf("resolveKVM(off) = %v, %v; want false, nil", got, err)
	}
	if got, err := resolveKVM("on", runtime.GOARCH); err != nil || !got {
		t.Errorf("resolveKVM(on) = %v, %v; want true, nil", got, err)
	}
	if _, err := resolveKVM("bogus", runtime.GOARCH); err == nil {
		t.Error("resolveKVM(bogus) = nil error, want error")
	}

	// auto on a guest arch that cannot match the host never enables (no KVM for
	// cross-architecture emulation), independent of /dev/kvm.
	otherArch := "arm64"
	if runtime.GOARCH == "arm64" {
		otherArch = "amd64"
	}
	if got, err := resolveKVM("auto", otherArch); err != nil || got {
		t.Errorf("resolveKVM(auto, cross-arch %q) = %v, %v; want false, nil", otherArch, got, err)
	}

	// auto on the host arch must not error (result depends on /dev/kvm presence).
	if _, err := resolveKVM("auto", runtime.GOARCH); err != nil {
		t.Errorf("resolveKVM(auto, host arch) error: %v", err)
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
