package cli

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

func TestDropFlag(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{nil, nil},
		{[]string{"-arch", "arm64"}, nil},
		{[]string{"-smp", "4", "-arch=arm64", "-v"}, []string{"-smp", "4", "-v"}},
		{[]string{"-v", "--arch", "s390x", "-env", "A=-arch"}, []string{"-v", "-env", "A=-arch"}},
		// A value that looks like the flag is not mistaken for it.
		{[]string{"-append", "-arch", "-keep"}, []string{"-append", "-arch", "-keep"}},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var vm vmFlags
		vm.register(fs)
		fs.String("arch", "", "")
		if err := fs.Parse(c.in); err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got := dropFlag(fs, c.in, "arch"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("dropFlag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
