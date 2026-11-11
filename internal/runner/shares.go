package runner

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// Share is a host directory exposed to the guest over virtio-9p.
type Share struct {
	HostPath  string // absolute host directory
	GuestPath string // absolute guest mount point
	ReadOnly  bool
}

// normalizeShares returns shares ordered by guest path, so a parent mounts
// before anything nested in it, with one share per guest path. Among duplicates
// a writable share wins, since something asked to write there.
func normalizeShares(shares []Share) []Share {
	out := slices.Clone(shares)
	slices.SortStableFunc(out, func(a, b Share) int {
		if c := cmp.Compare(a.GuestPath, b.GuestPath); c != 0 {
			return c
		}
		// Writable first, so Compact keeps it.
		switch {
		case a.ReadOnly == b.ReadOnly:
			return 0
		case a.ReadOnly:
			return 1
		}
		return -1
	})
	return slices.CompactFunc(out, func(a, b Share) bool { return a.GuestPath == b.GuestPath })
}

// shareTag is the 9p mount tag (and qemu fsdev id) of the i'th share.
func shareTag(i int) string { return fmt.Sprintf("share%d", i) }

// shareQEMUArgs returns the fsdev/device pair for each share. A writable share
// uses mapped-xattr, keeping guest ownership in xattrs so the host user can
// still read what the guest (root) writes.
func shareQEMUArgs(shares []Share) []string {
	var args []string
	for i, s := range shares {
		tag := shareTag(i)
		// qemu option values escape a literal comma by doubling it.
		fsdev := "local,id=" + tag + ",path=" + strings.ReplaceAll(s.HostPath, ",", ",,")
		if s.ReadOnly {
			fsdev += ",security_model=none,readonly=on"
		} else {
			fsdev += ",security_model=mapped-xattr"
		}
		args = append(args, "-fsdev", fsdev, "-device", "virtio-9p-pci,fsdev="+tag+",mount_tag="+tag)
	}
	return args
}

// shareList renders test-shares.txt for the guest init: one
// "tag<TAB>guest path<TAB>ro|rw" line per share, in mount order.
func shareList(shares []Share) string {
	lines := make([]string, len(shares))
	for i, s := range shares {
		mode := "rw"
		if s.ReadOnly {
			mode = "ro"
		}
		lines[i] = shareTag(i) + "\t" + s.GuestPath + "\t" + mode
	}
	return strings.Join(lines, "\n")
}
