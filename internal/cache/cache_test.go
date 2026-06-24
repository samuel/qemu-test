package cache

import "testing"

func TestCacheDir(t *testing.T) {
	t.Log(CacheDir())
}

func TestIsKernelURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"http://example.com/vmlinuz", true},
		{"https://example.com/vmlinuz", true},
		{"HTTP://example.com/vmlinuz", true}, // scheme is lower-cased by url.Parse
		{"/abs/path/vmlinuz", false},
		{"./rel/vmlinuz", false},
		{"vmlinuz", false},
		{"", false},
		{"ftp://example.com/vmlinuz", false},
		{"file:///vmlinuz", false},
	}
	for _, tc := range cases {
		if got := IsHTTPURL(tc.in); got != tc.want {
			t.Errorf("isKernelURL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCacheKey(t *testing.T) {
	a := CacheKey("http://example.com/vmlinuz")
	if a != CacheKey("http://example.com/vmlinuz") {
		t.Fatal("cacheKey not stable for the same URL")
	}
	if a == CacheKey("http://example.com/other") {
		t.Fatal("cacheKey collided for different URLs")
	}
	if len(a) != 64 { // sha256 hex
		t.Fatalf("cacheKey length = %d, want 64", len(a))
	}
}
