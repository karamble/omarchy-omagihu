// Package skill carries the agent skill that teaches an agent to arm, read and
// disarm omagihu alerts. The files are embedded so the skill installs from
// wherever the binary ended up, rather than depending on a source tree that may
// not be beside it.
package skill

import _ "embed"

// Name is the directory the skill is installed as, in both skill roots.
const Name = "omagihu-alerts"

//go:embed SKILL.md
var Doc string

//go:embed recipes.md
var Recipes string
