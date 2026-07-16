package bench

import "testing"

func TestSigHasParam(t *testing.T) {
	cases := []struct {
		sig, name, typ string
		want           bool
	}{
		{"func(a int, b string) int", "b", "string", true},
		{"func(a int) int", "b", "string", false},
		{"func(ctx context.Context, s github.com/hashicorp/vault/sdk/logical.Storage) error",
			"s", "logical.Storage", true}, // inspect qualifies with full paths
		{"func(s *github.com/x/logical.Storage)", "s", "*logical.Storage", true},
		{"func(a int, b string) int", "b", "int", false}, // right name, wrong type
		{"func(items []string)", "items", "[]string", true},
	}
	for _, c := range cases {
		if got := sigHasParam(c.sig, c.name, c.typ); got != c.want {
			t.Errorf("sigHasParam(%q, %q, %q) = %v, want %v", c.sig, c.name, c.typ, got, c.want)
		}
	}
}
