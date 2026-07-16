package sig

import "testing"

// TestScale is shaped exactly as add_test scaffolds a table-driven test, so
// the set_test_case/remove_test_case help-catalog examples have a real row
// ("one") to rewrite and delete. The row must keep the test passing:
// TestTestRunWholeWorkspace runs `go test ./...` over this fixture.
func TestScale(t *testing.T) {
	tests := []struct {
		name string
		v    int
		f    int
		want int
	}{
		{name: "one", v: 2, f: 1, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Scale(tt.v, tt.f); got != tt.want {
				t.Errorf("Scale(%v, %v) = %v, want %v", tt.v, tt.f, got, tt.want)
			}
		})
	}
}
