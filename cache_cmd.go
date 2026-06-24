package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/samuel/qemu-test/internal/cache"
)

// cacheCommand implements "qemu-test cache" — inspecting and pruning the on-disk
// kernel/bundle download cache. It returns a process exit code (0 success, 1 IO
// failure, 2 usage error).
func cacheCommand(args []string) int {
	fs := flag.NewFlagSet("qemu-test cache", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "In list, also print each item's on-disk paths")
	fs.Usage = cacheUsage(fs)

	// The sub-subcommand is a leading bare word; flags follow it (e.g.
	// "cache list -v"). A leading flag (e.g. "cache -h") applies to the default.
	verb := "info"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, rest = args[0], args[1:]
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	dir, err := cache.CacheDir()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		return 1
	}

	switch verb {
	case "dir":
		fmt.Println(dir)
		return 0
	case "info", "list":
		entries, err := cache.List(dir)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
			return 1
		}
		if verb == "list" {
			if err := printCacheList(entries, *verbose); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
				return 1
			}
		}
		printCacheSummary(dir, entries)
		return 0
	case "clear":
		return cacheClear(dir, fs.Args())
	default:
		_, _ = fmt.Fprintf(os.Stderr, "qemu-test cache: unknown subcommand %q\n", verb)
		fs.Usage()
		return 2
	}
}

// cacheClear removes cached items (all, or only those named by key or URL) and
// reports what was freed.
func cacheClear(dir string, targets []string) int {
	var keys []string
	for _, t := range targets {
		// Accept either a cache key (as shown by "list") or a source URL; a
		// downloaded entry's key is CacheKey(url), so map both forms to a key.
		keys = append(keys, t, cache.CacheKey(t))
	}
	removed, freed, err := cache.Clear(dir, keys...)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed: %s\n", err)
		return 1
	}
	if len(removed) == 0 {
		if len(targets) > 0 {
			_, _ = fmt.Fprintf(os.Stderr, "no matching cache item for %s\n", strings.Join(targets, ", "))
			return 1
		}
		fmt.Println("cache already empty")
		return 0
	}
	fmt.Printf("removed %d item(s), freed %s\n", len(removed), humanBytes(freed))
	return 0
}

// printCacheList writes the cache entries. The default is an aligned table keyed
// by a short key prefix; verbose prints a per-item block with the full key, URL,
// and on-disk paths.
func printCacheList(entries []cache.Entry, verbose bool) error {
	if len(entries) == 0 {
		return nil
	}
	if verbose {
		printCacheListVerbose(entries)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "KEY\tKIND\tSIZE\tFETCHED\tSOURCE"); err != nil {
		return err
	}
	for _, e := range entries {
		source := e.URL
		if source == "" {
			source = "-"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Key[:12], e.Kind, humanBytes(e.Size), fetchedAt(e), source); err != nil {
			return err
		}
	}
	return w.Flush()
}

// printCacheListVerbose prints a detailed block per entry.
func printCacheListVerbose(entries []cache.Entry) {
	for _, e := range entries {
		fmt.Printf("%s  %s  %s  %s\n", e.Key, e.Kind, humanBytes(e.Size), fetchedAt(e))
		if e.URL != "" {
			fmt.Printf("  url:   %s\n", e.URL)
		}
		for i, p := range e.Paths {
			label := "paths:"
			if i > 0 {
				label = "      "
			}
			fmt.Printf("  %s %s\n", label, p)
		}
		fmt.Println()
	}
}

// fetchedAt formats an entry's fetch time, or "-" when unknown.
func fetchedAt(e cache.Entry) string {
	if e.FetchedAt.IsZero() {
		return "-"
	}
	return e.FetchedAt.Local().Format("2006-01-02 15:04")
}

// printCacheSummary prints the item count and total size held under dir.
func printCacheSummary(dir string, entries []cache.Entry) {
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	fmt.Printf("%d item(s), %s total in %s\n", len(entries), humanBytes(total), dir)
}

// cacheUsage returns the usage function for the cache command.
func cacheUsage(fs *flag.FlagSet) func() {
	return func() {
		out := fs.Output()
		_, _ = fmt.Fprint(out, `qemu-test cache manages the on-disk kernel/bundle download cache.

Usage:
  qemu-test cache [info]                Show item count and total size (default)
  qemu-test cache list [-v]             List cached items (-v also shows paths)
  qemu-test cache clear [key|url ...]   Remove all cached items, or only those named
  qemu-test cache dir                   Print the cache directory path

Flags:
`)
		fs.PrintDefaults()
	}
}

// humanBytes formats a byte count with a binary (1024) unit suffix.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
