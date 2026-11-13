package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"samuelks.com/qemu-test/internal/bundler"
)

// bundleCommand implements "qemu-test bundle": it assembles a boot bundle (.tgz)
// from the sources a JSON config names, laid out the way -bundle expects. Returns
// a process exit code (0 success, 1 build failure, 2 usage error).
func bundleCommand(args []string) int {
	fs := flag.NewFlagSet("qemu-test bundle", flag.ContinueOnError)
	flagConfig := fs.String("config", "", "Path to the JSON bundle config")
	flagOut := fs.String("o", "", "Output bundle path (.tgz)")
	flagCache := fs.String("cache", "", "Download cache dir (default: the shared qemu-test cache dir)")
	flagRefresh := fs.Bool("refresh", false, "Re-validate cached downloads against the remote (conditional GET) and re-download if changed")
	flagVerbose := fs.Bool("v", false, "Enable verbose (debug) output")
	fs.Usage = bundleUsage(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *flagVerbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	if *flagConfig == "" || *flagOut == "" {
		_, _ = fmt.Fprintln(fs.Output(), "qemu-test bundle: -config and -o are required")
		fs.Usage()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := bundler.Run(ctx, *flagConfig, *flagOut, *flagCache, *flagRefresh); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		return 1
	}
	return 0
}

// bundleUsage returns the usage function for the bundle command.
func bundleUsage(fs *flag.FlagSet) func() {
	return func() {
		out := fs.Output()
		_, _ = fmt.Fprint(out, `qemu-test bundle assembles a boot bundle (.tgz) from remote files and archives.

Usage:
  qemu-test bundle -config <config.json> -o <bundle.tgz> [flags]

The config is a JSON object with one key "sources" whose value is an array of
sources. A plain source downloads one file to its "dst"; an archive source
selects files via "files" (glob -> dst). Globs use doublestar semantics: "*"
matches within a path segment, "**" any depth. A dst ending in "/" is a
directory: the matched path with the glob's fixed (non-wildcard) prefix removed
is appended under it. A "." segment in the glob is a keep-anchor (rsync -R):
only the part matched after it is kept, dropping everything before it, e.g.
"usr/lib/modules/*/./**" drops the version dir. http(s) URLs are downloaded and
cached; any other url value is read from the local filesystem.

Flags:
`)
		fs.PrintDefaults()
		_, _ = fmt.Fprint(out, `
Example config:
  {
    "sources": [
      {"url": "https://example.com/vmlinuz", "dst": "vmlinuz"},
      {"url": "https://example.com/initrd.gz", "archive": true,
       "files": {"usr/lib/modules/**": "modules/"}}
    ]
  }
`)
	}
}
