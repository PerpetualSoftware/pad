package main

import (
	"fmt"
	"os"

	"github.com/fatih/color"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/cli"
)

// writeSkill writes a tool's /pad skill through cli.WriteSkill, the one door
// that keeps an edited skill and refuses to downgrade a newer one (BUG-3466),
// and reports a kept file on stderr with the command that replaces it. It
// records the installation when it wrote.
func writeSkill(tool cli.AgentTool, force bool) (cli.SkillWriteResult, error) {
	res, err := cli.WriteSkill(tool, pad.PadSkill, version, force)
	if err != nil {
		return res, err
	}
	if res.Wrote {
		recordInstallation(tool.Name, res.Path)
	}
	reportKeptSkill(tool, res)
	return res, nil
}

// reportKeptSkill says why a skill file was left alone, and how to replace it.
func reportKeptSkill(tool cli.AgentTool, res cli.SkillWriteResult) {
	if res.Wrote {
		return
	}
	yellow := color.New(color.FgYellow)
	switch res.Action {
	case cli.SkillKeepEdited:
		yellow.Fprint(os.Stderr, "! ")
		fmt.Fprintf(os.Stderr, "Kept your edited /pad skill for %s (%s): it isn't what this pad wrote.\n  Replace it with: pad agent install %s --force\n",
			tool.Label, res.Path, tool.Name)
	case cli.SkillKeepNewer:
		yellow.Fprint(os.Stderr, "! ")
		fmt.Fprintf(os.Stderr, "Kept the /pad skill for %s (%s): pad %s wrote it, and this is pad %s.\n  Upgrade pad, or replace it with: pad agent install %s --force\n",
			tool.Label, res.Path, res.StampVersion, version, tool.Name)
	}
}
