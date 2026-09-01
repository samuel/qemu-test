package bundler

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// hasWildcard reports whether a path segment contains a glob metacharacter.
func hasWildcard(seg string) bool {
	return strings.ContainsAny(seg, "*?[")
}

// matchGlob reports whether name matches pattern using doublestar semantics:
// "*" matches within a single path segment, "**" any number of segments
// including none. Both are slash-separated and assumed already normalized.
func matchGlob(pattern, name string) (bool, error) {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

// matchSegments matches the remaining pattern segments against the remaining
// name segments.
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

// fixedPrefix returns the leading portion of pattern before its first wildcard
// segment. Stripping it from a matched name preserves the tree structure below
// the wildcard when joining onto a destination directory.
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
// style). glob is matched against entry names; when anchored, only the part of a
// matched name from segment lead onward is kept under a directory destination.
type parsedPattern struct {
	glob     string
	anchored bool
	lead     int
}

// parsePattern splits an optional "." keep-anchor out of a Source glob. Segments
// before the anchor must each consume exactly one name segment (no "**") so the
// kept remainder starts at a fixed index.
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

// mapDest computes the output path for a matched archive entry. A dst ending in
// "/" is a directory: an anchored pattern keeps the name from segment lead
// onward, otherwise the name with the glob's fixed prefix removed. Any other dst
// is the exact output path (the caller enforces a single match).
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
