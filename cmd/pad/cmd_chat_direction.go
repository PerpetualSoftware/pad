package main

import (
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/spf13/cobra"
)

const chatDirectionWarning = "Warning: instructions sent through Pad chat will drive this agent. Pad does not authenticate the sender; any process holding your Pad credential can send as you. This lasts for this session only. Delivery still needs pad session arm. Destructive or out-of-scope instructions should still be confirmed in-session."

func newChatArmCmd() *cobra.Command {
	var accept bool
	cmd := &cobra.Command{Use: "arm", Example: "  pad chat arm --accept-instructions", Short: "Accept chat instructions for this session", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !accept {
				return fmt.Errorf("chat arm requires --accept-instructions; use pad session arm for delivery")
			}
			fmt.Fprintln(cmd.ErrOrStderr(), chatDirectionWarning)
			path, err := cli.WriteChatDirection()
			if err != nil {
				return err
			}
			if err := cli.VerifyChatDirectionHeld(); err != nil {
				_ = cli.ClearChatDirection()
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{"chat_direction": true, "path": path, "warning": chatDirectionWarning})
			}
			fmt.Println("Chat instructions enabled for this session.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&accept, "accept-instructions", false, "Allow human chat messages to drive this agent for this session")
	return cmd
}

func newChatDisarmCmd() *cobra.Command {
	return &cobra.Command{Use: "disarm", Example: "  pad chat disarm", Short: "Treat chat as information again", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cli.ClearChatDirection(); err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{"chat_direction": false})
			}
			fmt.Println("Chat instructions disabled; delivery still follows pad session arm.")
			return nil
		},
	}
}
