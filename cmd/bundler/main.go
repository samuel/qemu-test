// Command bundler assembles a qemu-test boot bundle (.tgz) from remote
// files and archives: it downloads each source, selects files out of archives
// by glob, maps them to destination paths, and packs the result into a tgz laid
// out the way qemu-test's -bundle flag expects (vmlinuz plus a modules tree).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/samuel/qemu-test/internal/bundler"
)

func main() {
	flagConfig := flag.String("config", "", "Path to the JSON bundle config (an array of sources)")
	flagOut := flag.String("o", "", "Output bundle path (.tgz)")
	flagCache := flag.String("cache", "", "Download cache dir (default: the shared qemu-test cache dir)")
	flagRefresh := flag.Bool("refresh", false, "Re-validate cached downloads against the remote (conditional GET) and re-download if changed")
	flagVerbose := flag.Bool("v", false, "Enable verbose (debug) output")
	flag.Usage = usage
	flag.Parse()

	if *flagVerbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	if *flagConfig == "" || *flagOut == "" {
		_, _ = fmt.Fprintln(flag.CommandLine.Output(), "bundler: -config and -o are required")
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := bundler.Run(ctx, *flagConfig, *flagOut, *flagCache, *flagRefresh); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		os.Exit(1)
	}
}

// usage prints the help/usage message, wired in as flag.Usage.
func usage() {
	out := flag.CommandLine.Output()
	_, _ = fmt.Fprint(out, `bundler assembles a qemu-test boot bundle (.tgz) from remote files and archives.

Usage:
  bundler -config <config.json> -o <bundle.tgz> [flags]

The config is a JSON array of sources. A plain source downloads one file to its
"dst"; an archive source selects files via "files" (glob -> dst). Globs use
doublestar semantics: "*" matches within a path segment, "**" any depth. A dst
ending in "/" is a directory: the matched path with the glob's fixed (non-
wildcard) prefix removed is appended under it. A "." segment in the glob is a
keep-anchor (rsync -R): only the part matched after it is kept, dropping
everything before — e.g. "usr/lib/modules/*/./**" drops the version dir. http(s)
URLs are downloaded and cached; any other url value is read from the local
filesystem.

Flags:
`)
	flag.PrintDefaults()
	_, _ = fmt.Fprint(out, `
Example config:
  [
    {"url": "https://example.com/vmlinuz", "dst": "vmlinuz"},
    {"url": "https://example.com/initrd.gz", "archive": true,
     "files": {"usr/lib/modules/**": "modules/"}}
  ]
`)
}
