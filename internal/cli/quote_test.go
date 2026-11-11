package cli

import (
	"reflect"
	"testing"
)

func TestSplitQuoted(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{"   ", nil, false},
		{"-smp 4", []string{"-smp", "4"}, false},
		{"  -v\t-keep\n", []string{"-v", "-keep"}, false},
		{`-qemu.arg "-drive if=none,id=d0" -v`, []string{"-qemu.arg", "-drive if=none,id=d0", "-v"}, false},
		{`-append 'loglevel=7 quiet'`, []string{"-append", "loglevel=7 quiet"}, false},
		{`-env ''`, []string{"-env", ""}, false},
		{`-env KEY="a"`, []string{"-env", `KEY="a"`}, false},
		{`-append "unterminated`, nil, true},
	}
	for _, c := range cases {
		got, err := splitQuoted(c.in)
		if (err != nil) != c.wantErr || !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitQuoted(%q) = %q, %v, want %q (err %v)", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestJoinQuotedRoundTrips(t *testing.T) {
	cases := [][]string{
		{"/usr/bin/qemu-test"},
		{"/path with space/qemu-test", "-smp", "4"},
		{"qemu-test", "-qemu.arg", "-drive if=none,id=d0", "-env", ""},
		{"qemu-test", "-append", `it's quiet`},
		{"qemu-test", "-env", `"quoted"`},
	}
	for _, args := range cases {
		s, err := joinQuoted(args)
		if err != nil {
			t.Errorf("joinQuoted(%q): %v", args, err)
			continue
		}
		got, err := splitQuoted(s)
		if err != nil || !reflect.DeepEqual(got, args) {
			t.Errorf("splitQuoted(joinQuoted(%q) = %q) = %q, %v", args, s, got, err)
		}
	}
	if _, err := joinQuoted([]string{`a 'b' "c"`}); err == nil {
		t.Error("joinQuoted with both quote kinds: want an error")
	}
}
