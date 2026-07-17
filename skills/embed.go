// Package skills embeds the distributable agent skills so the binary can
// install them (ago skill install): every install channel gets the skill
// without a copy step, and the embedded copy cannot drift from the repo's.
package skills

import _ "embed"

//go:embed ago/SKILL.md
var Ago []byte
