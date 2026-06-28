package cli

import (
	"reflect"
	"testing"
)

func TestParseRawQemuArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"blank entries skipped", []string{"", "   "}, nil},
		{"single token, no value", []string{"-enable-kvm"}, []string{"-enable-kvm"}},
		{
			"flag and value split on first space",
			[]string{"-drive if=none,id=d0,file=disk.img,format=raw"},
			[]string{"-drive", "if=none,id=d0,file=disk.img,format=raw"},
		},
		{
			"only the first space splits; value keeps the rest",
			[]string{"-append console=ttyS0 quiet"},
			[]string{"-append", "console=ttyS0 quiet"},
		},
		{"surrounding whitespace trimmed", []string{"  -m   512  "}, []string{"-m", "512"}},
		{
			"multiple entries combined in order",
			[]string{"-enable-kvm", "-cpu host"},
			[]string{"-enable-kvm", "-cpu", "host"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRawQemuArgs(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseRawQemuArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
