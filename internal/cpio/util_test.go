package cpio

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"init", "init"},
		{"/init", "init"},
		{"///init", "init"},
		{"/a/b/c", "a/b/c"},
		{"a/b/", "a/b"},
		{"a/../b", "b"},
		{"./a", "a"},
		{"/foo/./bar", "foo/bar"},
		{"/", "."},
		{"", "."},
	}
	for _, tc := range tests {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRound4(t *testing.T) {
	tests := []struct {
		in, want int64
	}{
		{0, 0},
		{1, 4},
		{2, 4},
		{3, 4},
		{4, 4},
		{5, 8},
		{8, 8},
		{9, 12},
	}
	for _, tc := range tests {
		if got := round4(tc.in); got != tc.want {
			t.Errorf("round4(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
