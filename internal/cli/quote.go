package cli

import (
	"fmt"
	"strings"
)

// These follow the quoting rules `go test -exec` applies to its argument, so a
// string built by joinQuoted survives go's own splitting: fields are separated
// by whitespace, and a field starting with ' or " runs to the matching quote
// with no escapes inside.

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// splitQuoted splits s into fields.
func splitQuoted(s string) ([]string, error) {
	var fields []string
	for {
		s = strings.TrimLeft(s, " \t\n\r")
		if s == "" {
			return fields, nil
		}
		if q := s[0]; q == '"' || q == '\'' {
			end := strings.IndexByte(s[1:], q)
			if end < 0 {
				return nil, fmt.Errorf("unterminated %c string", q)
			}
			fields = append(fields, s[1:1+end])
			s = s[end+2:]
			continue
		}
		end := strings.IndexFunc(s, func(r rune) bool { return r < 0x80 && isSpaceByte(byte(r)) })
		if end < 0 {
			end = len(s)
		}
		fields = append(fields, s[:end])
		s = s[end:]
	}
}

// joinQuoted joins args into a string splitQuoted splits back into args. An
// argument needing quotes that contains both quote characters cannot be
// represented.
func joinQuoted(args []string) (string, error) {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		needsQuote := a == "" || a[0] == '"' || a[0] == '\'' || strings.ContainsAny(a, " \t\n\r")
		switch {
		case !needsQuote:
			b.WriteString(a)
		case !strings.Contains(a, "'"):
			b.WriteString("'" + a + "'")
		case !strings.Contains(a, `"`):
			b.WriteString(`"` + a + `"`)
		default:
			return "", fmt.Errorf("cannot quote argument containing both ' and \": %s", a)
		}
	}
	return b.String(), nil
}
