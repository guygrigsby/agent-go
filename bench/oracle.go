package bench

import "strings"

// zeroExpr picks a default expression for an added parameter's type when
// the oracle replays ground truth: call sites must typecheck, nothing
// more. ponytail: heuristic on the source type text; a task whose tests
// reject the zero value needs a mined default, recorded as an oracle
// finding rather than guessed here.
func zeroExpr(typ string) string {
	t := strings.TrimSpace(typ)
	switch {
	case t == "context.Context":
		return "context.Background()"
	case t == "string":
		return `""`
	case t == "bool":
		return "false"
	case strings.HasPrefix(t, "int"), strings.HasPrefix(t, "uint"),
		strings.HasPrefix(t, "float"), t == "byte", t == "rune":
		return "0"
	default:
		// Pointers, slices, maps, chans, funcs, and interfaces all take
		// nil; a named value type will reject at typecheck and surface as
		// an oracle finding.
		return "nil"
	}
}

// modesFor parses AGO_BENCH_MODES; empty means the raw-vs-semantic pair.
func modesFor(env string) []string {
	if env == "" {
		return []string{"raw", "semantic"}
	}
	var out []string
	for _, m := range strings.Split(env, ",") {
		out = append(out, strings.TrimSpace(m))
	}
	return out
}
