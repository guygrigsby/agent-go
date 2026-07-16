package sig

import (
	str "strings"
)

// Shout is self-contained apart from its aliased stdlib import, so it
// exercises move_decl's import carry: goimports can never reconstruct the
// str alias on its own.
func Shout(s string) string {
	return str.ToUpper(s)
}
