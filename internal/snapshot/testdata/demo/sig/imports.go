package sig

import (
	str "strings"

	"demo/lib"
)

// Shout is self-contained apart from its aliased stdlib import, so it
// exercises move_decl's import carry: goimports can never reconstruct the
// str alias on its own.
func Shout(s string) string {
	return str.ToUpper(s)
}

// MovedHome exercises qualifier stripping on move: relocated into demo/lib,
// its lib. qualifiers must become local references, never a self-import.
func MovedHome(s string) int {
	return lib.Limit + len(str.TrimSpace(s))
}
