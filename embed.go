package pad

import "embed"

//go:embed web/build/*
var WebUI embed.FS

//go:embed skills/pad/SKILL.md
var PadSkill []byte

// embed cache bust: 1774728494
