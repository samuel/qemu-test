package runner

import (
	"bytes"
	"reflect"
	"testing"
)

type rec struct {
	text    string
	partial bool
}

func TestStreamWriter(t *testing.T) {
	cases := []struct {
		name    string
		recs    []rec
		want    string
		wantQMP []string
	}{
		{"complete lines", []rec{{"a\n", false}, {"b\n", false}}, "a\nb\n", nil},
		{"chunk gets no newline", []rec{{"no newline\n", true}}, "no newline", nil},
		{"chunks join into a line", []rec{{"a\n", true}, {"b\n", true}, {"c\n", false}}, "abc\n", nil},
		{"qmp line is forwarded, not written", []rec{{"x\n", false}, {"[QMP]{\"execute\":\"stop\"}\n", false}, {"y\n", false}}, "x\ny\n", []string{`{"execute":"stop"}`}},
		{"qmp split over chunks", []rec{{"[Q\n", true}, {"MP]{}\n", true}, {"\n", false}}, "", []string{"{}"}},
		{"qmp-like prefix that is not qmp", []rec{{"[Q\n", true}, {"uoted]\n", false}}, "[Quoted]\n", nil},
		{"qmp only at line start", []rec{{"say \n", true}, {"[QMP]{}\n", false}}, "say [QMP]{}\n", nil},
		{"held text flushed at exit", []rec{{"[QM\n", true}}, "[QM", nil},
		{"partial keeps its own carriage return", []rec{{"50%\r\r\n", true}}, "50%\r", nil},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		var qmp []string
		s := &streamWriter{w: &buf, onQMP: func(cmd string) { qmp = append(qmp, cmd) }}
		for _, r := range c.recs {
			s.record([]byte(r.text), r.partial)
		}
		s.flush()
		if got := buf.String(); got != c.want || !reflect.DeepEqual(qmp, c.wantQMP) {
			t.Errorf("%s: wrote %q, qmp %q; want %q, qmp %q", c.name, got, qmp, c.want, c.wantQMP)
		}
	}
}

func TestStreamWriterWithoutQMP(t *testing.T) {
	var buf bytes.Buffer
	s := &streamWriter{w: &buf}
	s.record([]byte("[QMP]{}\n"), false)
	s.record([]byte("tail\n"), true)
	s.flush()
	if got, want := buf.String(), "[QMP]{}\ntail"; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
}
