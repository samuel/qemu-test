package cpio

import (
	"path"
	"strings"
)

const (
	FileTypeSocket           = 0o140000
	FileTypeSymbolicLink     = 0o120000
	FileTypeRegular          = 0o100000
	FileTypeBlockSpecial     = 0o060000
	FileTypeDirectory        = 0o040000
	FileTypeCharacterSpecial = 0o020000
	FileTypeNamedPipe        = 0o010000
)

// Normalize normalizes namepath to be relative to /.
func Normalize(name string) string {
	// do not use filepath.IsAbs, it will not work on Windows.
	// do not use filepath.Rel, that will not work
	// sensibly on windows.
	// The only thing one can do is strip all leading
	// /
	name = strings.TrimLeft(name, "/")
	// do not use filepath.Clean here.
	// This will result in paths with \\ on windows, and
	// / is the cpio standard.
	return path.Clean(name)
}

// round4 returns the next multiple of 4 close to n.
func round4(n int64) int64 {
	return (n + 3) &^ 0x3
}
