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

// Counter exercises the dependency-closure move: the type and its methods
// travel together.
type Counter struct{ n int }

func (c *Counter) Add(v int) { c.n += v }

func (c *Counter) Total() int {
	c.Add(0)
	return c.n
}

// Tainted has a method leaning on a package-local function, so its
// closure move must reject with the dependency named.
type Tainted struct{}

func (Tainted) Use() int { return UseFetch() }

// NewCounter is Counter's constructor: an intra-set dependency when the
// two move together.
func NewCounter() *Counter { return &Counter{} }
