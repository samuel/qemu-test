package bundler

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// hasWildcard reports whether a single path segment contains a glob
// metacharacter that path.Match interprets.
func hasWildcard(seg string) bool {
	return strings.ContainsAny(seg, "*?[")
}

// matchGlob reports whether name matches pattern using doublestar semantics:
// "*" (and the other path.Match metacharacters) match within a single path
// segment, while "**" matches any number of segments including none. Both
// pattern and name are slash-separated and assumed already normalized (no
// leading "./" or "/").
func matchGlob(pattern, name string) (bool, error) {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

// matchSegments matches the remaining pattern segments against the remaining
// name segments. A "**" segment matches zero or more name segments; every other
// segment matches exactly one name segment via path.Match.
func matchSegments(pat, name []string) (bool, error) {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return true, nil // trailing ** swallows everything left
			}
			for i := range len(name) + 1 {
				ok, err := matchSegments(rest, name[i:])
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			}
			return false, nil
		}
		if len(name) == 0 {
			return false, nil
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0, nil
}

// fixedPrefix returns the leading slash-separated portion of pattern up to (but
// excluding) the first segment that contains a wildcard, with no trailing
// slash. It is the part of a matched name stripped before the remainder is
// joined onto a destination directory, so a tree's structure below the wildcard
// is preserved.
func fixedPrefix(pattern string) string {
	var fixed []string
	for seg := range strings.SplitSeq(pattern, "/") {
		if hasWildcard(seg) {
			break
		}
		fixed = append(fixed, seg)
	}
	return strings.Join(fixed, "/")
}

// parsedPattern is a Source glob split at an optional "." keep-anchor (rsync -R
// style). glob is the anchor-free pattern matched against entry names. When
// anchored, the part of a matched name from segment lead onward is the relative
// path kept under a directory destination, so everything before the "." — a
// variable version dir matched by "*", say — is dropped.
type parsedPattern struct {
	glob     string
	anchored bool
	lead     int
}

// parsePattern splits an optional "." keep-anchor out of a Source glob. The "."
// segment marks where the kept path begins; segments before it must each consume
// exactly one name segment (no "**"), so the kept remainder starts at a fixed
// index.
func parsePattern(p string) (parsedPattern, error) {
	anchor := -1
	var glob []string
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "":
			continue // tolerate "//" and trailing "/"
		case ".":
			if anchor >= 0 {
				return parsedPattern{}, fmt.Errorf("pattern %q has more than one %q keep-anchor", p, ".")
			}
			anchor = len(glob) // glob segments seen so far == segments before the anchor
		default:
			glob = append(glob, seg)
		}
	}
	pp := parsedPattern{glob: strings.Join(glob, "/")}
	if anchor < 0 {
		return pp, nil
	}
	if slices.Contains(glob[:anchor], "**") {
		return parsedPattern{}, fmt.Errorf("pattern %q: %q before the %q keep-anchor is not allowed (the cut point would be ambiguous)", p, "**", ".")
	}
	pp.anchored, pp.lead = true, anchor
	return pp, nil
}

// mapDest computes the output path for a matched archive entry. name is the
// (normalized) entry name and dst the configured destination. A dst ending in
// "/" is a directory: an anchored pattern keeps the name from segment lead
// onward, otherwise the entry's path with pp.glob's fixed prefix removed is
// appended under it. A dst without a trailing "/" is the exact output path (the
// caller enforces that such a pattern matches one entry).
func mapDest(pp parsedPattern, dst, name string) string {
	if !strings.HasSuffix(dst, "/") {
		return dst
	}
	var rel string
	switch {
	case pp.anchored:
		segs := strings.Split(name, "/")
		if pp.lead >= len(segs) {
			rel = path.Base(name) // anchor past the end (not reachable for files); keep the basename
		} else {
			rel = strings.Join(segs[pp.lead:], "/")
		}
	default:
		rel = name
		if pre := fixedPrefix(pp.glob); pre != "" {
			rel = strings.TrimPrefix(name, pre+"/")
		}
	}
	return path.Clean(dst + rel)
}
