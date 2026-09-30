package pad

import "embed"

// Embedded frontend and built-in skill assets.

//go:embed all:web/build
var WebUI embed.FS

//go:embed skills/pad/SKILL.md
var PadSkill []byte

// MaterializerJS is the headless op-log materializer bundle (TASK-2198),
// built by web/scripts/build-materializer.mjs (part of `npm run build`) and
// run by internal/materialize. Generated and gitignored, like web/build.
//
//go:embed web/build-materializer/materializer.js
var MaterializerJS []byte
