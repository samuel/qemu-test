package bundler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractABI(t *testing.T) {
	cases := []struct {
		name    string
		depends string
		abiDep  string
		want    string
		wantErr bool
	}{
		{
			name:    "simple with version constraint",
			depends: "linux-image-6.12.86+deb13-amd64 (= 6.12.86-1)",
			abiDep:  "linux-image-{abi}-amd64",
			want:    "6.12.86+deb13",
		},
		{
			name:    "picked out of several deps",
			depends: "linux-base-powerpc64 (= 7.0.12-1), linux-image-7.0.12+deb14-powerpc64 (= 7.0.12-1)",
			abiDep:  "linux-image-{abi}-powerpc64",
			want:    "7.0.12+deb14",
		},
		{
			name:    "alternative takes first name",
			depends: "linux-image-6.12-amd64 | linux-image-amd64",
			abiDep:  "linux-image-{abi}-amd64",
			want:    "6.12",
		},
		{
			name:    "no matching dependency",
			depends: "initramfs-tools | linux-initramfs-tool",
			abiDep:  "linux-image-{abi}-amd64",
			wantErr: true,
		},
		{
			name:    "empty abi is rejected (meta-package self-name)",
			depends: "linux-image-amd64",
			abiDep:  "linux-image-{abi}-amd64",
			wantErr: true,
		},
		{
			name:    "template without placeholder",
			depends: "linux-image-6.12-amd64",
			abiDep:  "linux-image-amd64",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractABI(tc.depends, tc.abiDep)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("extractABI(%q, %q) = %q, want error", tc.depends, tc.abiDep, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractABI(%q, %q): %v", tc.depends, tc.abiDep, err)
			}
			if got != tc.want {
				t.Errorf("extractABI(%q, %q) = %q, want %q", tc.depends, tc.abiDep, got, tc.want)
			}
		})
	}
}

func TestParsePackages(t *testing.T) {
	index := strings.Join([]string{
		"Package: linux-image-amd64",
		"Version: 6.12.86-1",
		"Depends: linux-image-6.12.86+deb13-amd64 (= 6.12.86-1)",
		"Description: meta",
		" continuation line",
		"",
		"Package: linux-image-6.12.86+deb13-amd64-unsigned",
		"Filename: pool/main/l/linux/the-image.deb",
		"",
		"", // trailing blank lines must not create an empty stanza
	}, "\n")
	pkgs, err := parsePackages([]byte(index))
	if err != nil {
		t.Fatalf("parsePackages: %v", err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("parsed %d packages, want 2: %v", len(pkgs), pkgs)
	}
	meta := pkgs["linux-image-amd64"]
	if got := meta["Depends"]; got != "linux-image-6.12.86+deb13-amd64 (= 6.12.86-1)" {
		t.Errorf("Depends = %q", got)
	}
	if got := meta["Description"]; got != "meta\ncontinuation line" {
		t.Errorf("folded Description = %q, want the continuation joined", got)
	}
	if got := pkgs["linux-image-6.12.86+deb13-amd64-unsigned"]["Filename"]; got != "pool/main/l/linux/the-image.deb" {
		t.Errorf("Filename = %q", got)
	}
}

func TestParsePackagesEmpty(t *testing.T) {
	if _, err := parsePackages([]byte("\n\n")); err == nil {
		t.Error("parsePackages accepted an index with no stanzas")
	}
}

// debianServer serves a Debian-style repo: a binary-<arch> Packages index (named
// by ext, e.g. ".xz") plus the pool .deb it points at. indexBody is the raw
// (uncompressed) index; the handler is registered under the compressed name only.
func debianServer(t *testing.T, arch, indexExt string, indexBody, deb []byte, debPoolPath string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/dists/trixie/main/binary-"+arch+"/Packages"+indexExt, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(indexBody)
	})
	mux.HandleFunc("/"+debPoolPath, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(deb)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestBundleDebianEndToEnd(t *testing.T) {
	const abi = "6.12.86+deb13"
	debPool := "pool/main/l/linux/linux-image-" + abi + "-amd64-unsigned_6.12.86-1_amd64.deb"
	// The .deb's data member holds the installed layout (./-prefixed, as dpkg ships).
	deb := debBytes(t, "data.tar.xz", xzBytes(t, tarBytes(t, false, map[string]string{
		"./boot/vmlinuz-" + abi + "-amd64":                   "KERNEL-IMAGE",
		"./usr/lib/modules/" + abi + "-amd64/modules.dep":    "deps",
		"./usr/lib/modules/" + abi + "-amd64/kernel/x.ko.xz": "ko",
	})))
	index := strings.Join([]string{
		"Package: linux-image-amd64",
		"Depends: linux-image-" + abi + "-amd64 (= 6.12.86-1)",
		"",
		"Package: linux-image-" + abi + "-amd64-unsigned",
		"Filename: " + debPool,
		"",
	}, "\n")

	// Index only as .gz here, so the .xz fetch 404s and the .gz fallback is exercised.
	srv := debianServer(t, "amd64", ".gz", maybeGzip(t, true, []byte(index)), deb, debPool)

	cfg := &Config{Sources: []Source{{
		Debian: &DebianRef{
			Mirror:    srv.URL,
			Suite:     "trixie",
			Component: "main",
			Arch:      "amd64",
			Via:       "linux-image-amd64",
			AbiDep:    "linux-image-{abi}-amd64",
			Package:   "linux-image-{abi}-amd64-unsigned",
		},
		Archive: true,
		Files: map[string]string{
			"boot/vmlinuz-*":         "vmlinuz",
			"usr/lib/modules/*/./**": "modules/",
		},
	}}}

	out := filepath.Join(t.TempDir(), "boot.tgz")
	if err := Bundle(context.Background(), cfg, out, t.TempDir(), false); err != nil {
		t.Fatalf("bundle: %v", err)
	}
	want := map[string]string{
		"vmlinuz":                "KERNEL-IMAGE",
		"modules/modules.dep":    "deps",
		"modules/kernel/x.ko.xz": "ko",
	}
	got := extractBundle(t, out)
	if len(got) != len(want) {
		t.Fatalf("bundle = %v, want %v", keys(got), keys(want))
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("bundle[%q] = %q, want %q", name, got[name], body)
		}
	}
}

func TestBundleDebianResolveErrors(t *testing.T) {
	index := strings.Join([]string{
		"Package: linux-image-amd64",
		"Depends: linux-image-6.12-amd64 (= 1)",
		"",
		// note: the -unsigned target stanza is intentionally absent
	}, "\n")
	srv := debianServer(t, "amd64", ".xz", xzBytes(t, []byte(index)), nil, "unused")
	ref := func(mut func(*DebianRef)) *DebianRef {
		d := &DebianRef{
			Mirror: srv.URL, Suite: "trixie", Component: "main", Arch: "amd64",
			Via: "linux-image-amd64", AbiDep: "linux-image-{abi}-amd64",
			Package: "linux-image-{abi}-amd64-unsigned",
		}
		mut(d)
		return d
	}
	cases := map[string]struct {
		ref  *DebianRef
		want string
	}{
		"missing meta-package":  {ref(func(d *DebianRef) { d.Via = "linux-image-nope" }), "not found"},
		"missing target stanza": {ref(func(*DebianRef) {}), "not found"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := &Config{Sources: []Source{{Debian: tc.ref, Archive: true, Files: map[string]string{"x": "y"}}}}
			err := Bundle(context.Background(), cfg, filepath.Join(t.TempDir(), "o.tgz"), t.TempDir(), false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfigDebianValidation(t *testing.T) {
	cases := map[string]string{
		"url and debian together": `{"sources": [{"url": "u", "debian": {"mirror":"m","suite":"s","component":"c","arch":"a","via":"v","abiDep":"x-{abi}","package":"p-{abi}"}, "dst": "v"}]}`,
		"debian missing field":    `{"sources": [{"debian": {"mirror":"m","suite":"s","component":"c","arch":"a","via":"v","package":"p-{abi}"}, "dst": "v"}]}`,
		"abiDep no placeholder":   `{"sources": [{"debian": {"mirror":"m","suite":"s","component":"c","arch":"a","via":"v","abiDep":"x","package":"p-{abi}"}, "dst": "v"}]}`,
		"package no placeholder":  `{"sources": [{"debian": {"mirror":"m","suite":"s","component":"c","arch":"a","via":"v","abiDep":"x-{abi}","package":"p"}, "dst": "v"}]}`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, cfg)); err == nil {
				t.Errorf("loadConfig accepted invalid config %q", cfg)
			}
		})
	}
}

func TestLoadConfigDebianValid(t *testing.T) {
	cfg := `{"sources": [{"debian": {"mirror":"https://m","suite":"trixie","component":"main","arch":"amd64","via":"linux-image-amd64","abiDep":"linux-image-{abi}-amd64","package":"linux-image-{abi}-amd64-unsigned"}, "archive": true, "files": {"boot/vmlinuz-*":"vmlinuz"}}]}`
	if _, err := LoadConfig(writeConfig(t, cfg)); err != nil {
		t.Fatalf("loadConfig rejected a valid debian config: %v", err)
	}
}
