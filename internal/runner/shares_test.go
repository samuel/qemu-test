package runner

import (
	"reflect"
	"testing"
)

func TestNormalizeShares(t *testing.T) {
	in := []Share{
		{HostPath: "/h/pkg/sub", GuestPath: "/h/pkg/sub"},
		{HostPath: "/h/pkg", GuestPath: "/h/pkg", ReadOnly: true},
		{HostPath: "/h/pkg", GuestPath: "/h/pkg"},
		{HostPath: "/h/out", GuestPath: "/a", ReadOnly: true},
	}
	want := []Share{
		{HostPath: "/h/out", GuestPath: "/a", ReadOnly: true},
		{HostPath: "/h/pkg", GuestPath: "/h/pkg"},
		{HostPath: "/h/pkg/sub", GuestPath: "/h/pkg/sub"},
	}
	if got := normalizeShares(in); !reflect.DeepEqual(got, want) {
		t.Errorf("normalizeShares = %+v, want %+v", got, want)
	}
	if got := normalizeShares(nil); len(got) != 0 {
		t.Errorf("normalizeShares(nil) = %+v", got)
	}
}

func TestShareArgsAndList(t *testing.T) {
	shares := []Share{
		{HostPath: "/h/a,b", GuestPath: "/a", ReadOnly: true},
		{HostPath: "/h/out", GuestPath: "/out"},
	}
	wantArgs := []string{
		"-fsdev", "local,id=share0,path=/h/a,,b,security_model=none,readonly=on",
		"-device", "virtio-9p-pci,fsdev=share0,mount_tag=share0",
		"-fsdev", "local,id=share1,path=/h/out,security_model=mapped-xattr",
		"-device", "virtio-9p-pci,fsdev=share1,mount_tag=share1",
	}
	if got := shareQEMUArgs(shares); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("shareQEMUArgs = %q, want %q", got, wantArgs)
	}
	if got, want := shareList(shares), "share0\t/a\tro\nshare1\t/out\trw"; got != want {
		t.Errorf("shareList = %q, want %q", got, want)
	}
}

func TestVerdictRecord(t *testing.T) {
	cases := []struct {
		key, line string
		want      verdict
		wantOK    bool
	}{
		{"EXIT", "0\n", verdict{exited: true}, true},
		{"EXIT", "3\r\n", verdict{exited: true, code: 3}, true},
		{"EXIT", "x\n", verdict{failure: `malformed exit status "x"`}, true},
		{"FAIL", "signal: killed\n", verdict{failure: "signal: killed"}, true},
		{"FAIL", "\n", verdict{failure: "failed"}, true},
		{"BOGUS", "x\n", verdict{}, false},
	}
	for _, c := range cases {
		var v verdict
		if ok := v.record(c.key, []byte(c.line)); ok != c.wantOK || v != c.want {
			t.Errorf("record(%q, %q) = %+v, %v, want %+v, %v", c.key, c.line, v, ok, c.want, c.wantOK)
		}
	}
}
