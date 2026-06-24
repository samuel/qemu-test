package bundler

import "testing"

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"usr/lib/modules/**", "usr/lib/modules/6.12/kernel/x.ko", true},
		{"usr/lib/modules/**", "usr/lib/modules/modules.dep", true},
		{"usr/lib/modules/**", "usr/lib/modules", true}, // ** matches zero segments
		{"usr/lib/modules/**", "usr/lib/other/x.ko", false},
		{"usr/lib/modules/*", "usr/lib/modules/6.12", true},
		{"usr/lib/modules/*", "usr/lib/modules/6.12/x.ko", false}, // * is one segment
		{"usr/lib/modules/*/**", "usr/lib/modules/6.12/kernel/x.ko", true},
		{"kernel/drivers/*.ko", "kernel/drivers/e1000.ko", true},
		{"kernel/drivers/*.ko", "kernel/drivers/net/e1000.ko", false},
		{"**/*.ko", "a/b/c.ko", true},
		{"**/*.ko", "c.ko", true}, // ** matches zero, then *.ko
		{"**", "anything/here/deep", true},
		{"a/b", "a/b", true},
		{"a/b", "a/c", false},
		{"*", "file", true},
		{"*", "a/b", false},
	}
	for _, tc := range cases {
		got, err := matchGlob(tc.pattern, tc.name)
		if err != nil {
			t.Errorf("matchGlob(%q, %q) error: %v", tc.pattern, tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestFixedPrefix(t *testing.T) {
	cases := map[string]string{
		"usr/lib/modules/**":   "usr/lib/modules",
		"usr/lib/modules/*/**": "usr/lib/modules",
		"kernel/drivers/*.ko":  "kernel/drivers",
		"*.ko":                 "",
		"**":                   "",
		"a/b/c":                "a/b/c",
	}
	for pattern, want := range cases {
		if got := fixedPrefix(pattern); got != want {
			t.Errorf("fixedPrefix(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func mustParse(t *testing.T, p string) parsedPattern {
	t.Helper()
	pp, err := parsePattern(p)
	if err != nil {
		t.Fatalf("parsePattern(%q): %v", p, err)
	}
	return pp
}

func TestParsePattern(t *testing.T) {
	cases := []struct {
		in       string
		glob     string
		anchored bool
		lead     int
	}{
		{"usr/lib/modules/**", "usr/lib/modules/**", false, 0},
		{"usr/lib/modules/*/./**", "usr/lib/modules/*/**", true, 4},
		{"usr/lib/modules/*/./kernel/**", "usr/lib/modules/*/kernel/**", true, 4},
		{"./**", "**", true, 0},
		{"usr//lib/modules/**/", "usr/lib/modules/**", false, 0}, // empty segments tolerated
	}
	for _, tc := range cases {
		pp, err := parsePattern(tc.in)
		if err != nil {
			t.Errorf("parsePattern(%q): %v", tc.in, err)
			continue
		}
		if pp.glob != tc.glob || pp.anchored != tc.anchored || pp.lead != tc.lead {
			t.Errorf("parsePattern(%q) = %+v, want {glob:%q anchored:%v lead:%d}", tc.in, pp, tc.glob, tc.anchored, tc.lead)
		}
	}

	for _, bad := range []string{"a/./b/./c", "usr/lib/modules/**/./x"} {
		if _, err := parsePattern(bad); err == nil {
			t.Errorf("parsePattern(%q) = nil error, want failure", bad)
		}
	}
}

func TestMapDest(t *testing.T) {
	cases := []struct {
		pattern, dst, name, want string
	}{
		{"usr/lib/modules/**", "lib/modules/", "usr/lib/modules/6.12/kernel/x.ko", "lib/modules/6.12/kernel/x.ko"},
		{"usr/lib/modules/**", "modules/", "usr/lib/modules/6.12/modules.dep", "modules/6.12/modules.dep"},
		{"usr/lib/modules/*/**", "lib/modules/", "usr/lib/modules/6.12/kernel/x.ko", "lib/modules/6.12/kernel/x.ko"},
		{"*.ko", "mods/", "e1000.ko", "mods/e1000.ko"},
		{"vmlinuz", "boot/vmlinuz", "vmlinuz", "boot/vmlinuz"}, // non-dir dst is exact
		// "." keep-anchor drops the version dir matched by "*".
		{"usr/lib/modules/*/./**", "modules/", "usr/lib/modules/6.12.86+deb13-armmp/modules.builtin", "modules/modules.builtin"},
		{"usr/lib/modules/*/./**", "modules/", "usr/lib/modules/6.12.86+deb13-armmp/kernel/net/e1000.ko", "modules/kernel/net/e1000.ko"},
		{"usr/lib/modules/*/./kernel/**", "modules/", "usr/lib/modules/6.12/kernel/net/e1000.ko", "modules/kernel/net/e1000.ko"},
	}
	for _, tc := range cases {
		if got := mapDest(mustParse(t, tc.pattern), tc.dst, tc.name); got != tc.want {
			t.Errorf("mapDest(%q, %q, %q) = %q, want %q", tc.pattern, tc.dst, tc.name, got, tc.want)
		}
	}
}
