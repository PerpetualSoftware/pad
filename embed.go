package pad

import "embed"

// Embedded frontend and built-in skill assets.

//go:embed all:web/build
var WebUI embed.FS

//go:embed skills/pad/SKILL.md
var PadSkill []byte
