package bench

import (
	"errors"
	"testing"
)

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		timedOut bool
		out      string
		pass     bool
		want     string
	}{
		{"pass", nil, false, "", true, ""},
		{"capped wins over error", errors.New("killed"), true, "", false, "capped"},
		{"endpoint refused", errors.New("exit 1"), false, "dial tcp: connection refused", false, "endpoint_error"},
		{"endpoint 503", errors.New("exit 1"), false, "unexpected status 503 from provider", false, "endpoint_error"},
		{"harness crash", errors.New("exit 2"), false, "panic: runtime error", false, "harness_crash"},
		{"clean run scored fail", nil, false, "done", false, "scored_fail"},
	}
	for _, c := range cases {
		if got := classifyFailure(c.err, c.timedOut, c.out, c.pass); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
