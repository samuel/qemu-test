package cli

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"samuelks.com/qemu-test/internal/runner"
)

func TestTestTimeout(t *testing.T) {
	cases := []struct {
		args   []string
		want   time.Duration
		wantOK bool
	}{
		{nil, 0, false},
		{[]string{"-test.v"}, 0, false},
		{[]string{"-test.paniconexit0", "-test.timeout=10m0s"}, 10 * time.Minute, true},
		{[]string{"--test.timeout", "30s"}, 30 * time.Second, true},
		{[]string{"-test.timeout=0"}, 0, true},
		{[]string{"-test.timeout=bogus"}, 0, false},
		// Past the flags, an argument is the binary's own.
		{[]string{"arg", "-test.timeout=1s"}, 0, false},
		{[]string{"--", "-test.timeout=1s"}, 0, false},
	}
	for _, c := range cases {
		got, ok := testTimeout(c.args)
		if got != c.want || ok != c.wantOK {
			t.Errorf("testTimeout(%q) = %v, %v, want %v, %v", c.args, got, ok, c.want, c.wantOK)
		}
	}
}

func TestMirrorShares(t *testing.T) {
	dir := t.TempDir()
	rw := func(p string) runner.Share { return runner.Share{HostPath: p, GuestPath: p} }

	cases := []struct {
		name          string
		args          []string
		want          []runner.Share
		wantWritesCwd bool
	}{
		{"no path flags", []string{"-test.v", "-test.timeout=1m"}, nil, false},
		{"absolute dir flag", []string{"-test.gocoverdir=" + dir}, []runner.Share{rw(dir)}, false},
		{"absolute file flag shares its dir", []string{"-test.coverprofile=" + filepath.Join(dir, "c.out")}, []runner.Share{rw(dir)}, false},
		{"relative file resolves against outputdir", []string{"-test.outputdir=" + dir, "-test.cpuprofile", "cpu.out"}, []runner.Share{rw(dir), rw(dir)}, false},
		{"relative file without outputdir writes cwd", []string{"-test.coverprofile=c.out"}, nil, true},
		{"missing host dir is not shared", []string{"-test.gocoverdir=" + filepath.Join(dir, "nope")}, nil, false},
	}
	for _, c := range cases {
		got, writesCwd := mirrorShares(c.args)
		if !reflect.DeepEqual(got, c.want) || writesCwd != c.wantWritesCwd {
			t.Errorf("%s: mirrorShares(%q) = %v, %v, want %v, %v", c.name, c.args, got, writesCwd, c.want, c.wantWritesCwd)
		}
	}
}

func TestParseShare(t *testing.T) {
	dir := t.TempDir()
	newDir := filepath.Join(dir, "new")

	cases := []struct {
		spec    string
		want    runner.Share
		wantErr bool
	}{
		{dir, runner.Share{HostPath: dir, GuestPath: dir}, false},
		{dir + ":ro", runner.Share{HostPath: dir, GuestPath: dir, ReadOnly: true}, false},
		{dir + ":/out", runner.Share{HostPath: dir, GuestPath: "/out"}, false},
		{dir + ":/out:ro", runner.Share{HostPath: dir, GuestPath: "/out", ReadOnly: true}, false},
		{newDir + ":/out:rw", runner.Share{HostPath: newDir, GuestPath: "/out"}, false},
		{dir + ":relative", runner.Share{}, true},
		{filepath.Join(dir, "missing") + ":ro", runner.Share{}, true},
		{"", runner.Share{}, true},
		{dir + ":/a:/b", runner.Share{}, true},
	}
	for _, c := range cases {
		got, err := parseShare(c.spec)
		if (err != nil) != c.wantErr {
			t.Errorf("parseShare(%q) error = %v, wantErr %v", c.spec, err, c.wantErr)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("parseShare(%q) = %+v, want %+v", c.spec, got, c.want)
		}
	}
}

func TestPlanShares(t *testing.T) {
	cwd := t.TempDir()
	share := func(ro bool) runner.Share { return runner.Share{HostPath: cwd, GuestPath: cwd, ReadOnly: ro} }

	cases := []struct {
		name        string
		mode        string
		args        []string
		wantWorkDir string
		want        []runner.Share
	}{
		{"default shares cwd read-only", cwdReadOnly, nil, cwd, []runner.Share{share(true)}},
		{"rw", cwdReadWrite, nil, cwd, []runner.Share{share(false)}},
		{"none", cwdNone, nil, "", nil},
		{"relative output upgrades cwd to writable", cwdReadOnly, []string{"-test.coverprofile=c.out"}, cwd, []runner.Share{share(false)}},
	}
	for _, c := range cases {
		workDir, got, err := planShares(nil, c.mode, cwd, c.args)
		if err != nil || workDir != c.wantWorkDir || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: planShares = %q, %v, %v, want %q, %v", c.name, workDir, got, err, c.wantWorkDir, c.want)
		}
	}
}
