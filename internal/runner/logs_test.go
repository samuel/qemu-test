package runner

import (
	"reflect"
	"testing"
)

// parseAll feeds chunks to a fresh logParser (then flushes) and returns the
// (key, line) records the callback observed. Strings are copied immediately, as
// the callback's byte slices alias the parser's internal buffer.
func parseAll(t *testing.T, chunks ...string) [][2]string {
	t.Helper()
	var recs [][2]string
	lp := &logParser{fn: func(key, line []byte) error {
		recs = append(recs, [2]string{string(key), string(line)})
		return nil
	}}
	for _, c := range chunks {
		if _, err := lp.Write([]byte(c)); err != nil {
			t.Fatalf("Write(%q): %v", c, err)
		}
	}
	if err := lp.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return recs
}

// keys returns just the control keys observed (empty keys are plain output).
func keys(recs [][2]string) []string {
	var ks []string
	for _, r := range recs {
		if r[0] != "" {
			ks = append(ks, r[0])
		}
	}
	return ks
}

// Regression for the bug where Write processed only the first complete line per
// call: a chunk batching several lines and ending in a [TESTS:PASS] marker must
// surface every line, not strand the verdict.
func TestLogParserProcessesAllLinesInOneWrite(t *testing.T) {
	recs := parseAll(t, "[TESTS:STDOUT] one\n[TESTS:STDOUT] two\n[TESTS:PASS]\n")
	if got, want := keys(recs), []string{"STDOUT", "STDOUT", "PASS"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v (records: %v)", got, want, recs)
	}
}

func TestLogParserStripsControlPrefix(t *testing.T) {
	recs := parseAll(t, "[TESTS:STDOUT] hello world\n")
	want := [][2]string{{"STDOUT", "hello world\n"}}
	if !reflect.DeepEqual(recs, want) {
		t.Fatalf("recs = %v, want %v", recs, want)
	}
}

// A marker split across two writes must still be recognized once completed.
func TestLogParserHandlesSplitWrites(t *testing.T) {
	recs := parseAll(t, "[TESTS:PA", "SS]\n")
	if got, want := keys(recs), []string{"PASS"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v (records: %v)", got, want, recs)
	}
}

// flush must recover a final line emitted without a trailing newline (the VM
// can power off immediately after printing the verdict).
func TestLogParserFlushesTrailingLine(t *testing.T) {
	recs := parseAll(t, "[TESTS:STDOUT] partial\n[TESTS:FAIL] boom")
	if got, want := keys(recs), []string{"STDOUT", "FAIL"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v (records: %v)", got, want, recs)
	}
}

func TestLogParserNonControlLine(t *testing.T) {
	recs := parseAll(t, "kernel boot noise\n")
	want := [][2]string{{"", "kernel boot noise\n"}}
	if !reflect.DeepEqual(recs, want) {
		t.Fatalf("recs = %v, want %v", recs, want)
	}
}
