package bench

import (
	"regexp"
	"strings"
)

// qualifier strips package qualifiers from a type string so the extractor's
// source-side rendering ("logical.Storage") and inspect's full-path
// rendering ("github.com/.../logical.Storage") compare equal by base type.
var qualifier = regexp.MustCompile(`[\w./\-]+\.`)

func typeBase(t string) string {
	return qualifier.ReplaceAllString(strings.TrimSpace(t), "")
}

// sigHasParam reports whether a types.TypeString signature declares a
// parameter with this name and (base-)type.
func sigHasParam(sig, name, typ string) bool {
	open := strings.IndexByte(sig, '(')
	if open < 0 {
		return false
	}
	// The parameter list is the top-level paren group; scan it splitting on
	// depth-0 commas.
	depth, start := 0, open+1
	var params []string
	for i := open; i < len(sig); i++ {
		switch sig[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				params = append(params, sig[start:i])
				i = len(sig)
			}
		case ',':
			if depth == 1 {
				params = append(params, sig[start:i])
				start = i + 1
			}
		}
	}
	for _, p := range params {
		fields := strings.SplitN(strings.TrimSpace(p), " ", 2)
		if len(fields) != 2 {
			continue
		}
		if fields[0] == name && typeBase(fields[1]) == typeBase(typ) {
			return true
		}
	}
	return false
}
