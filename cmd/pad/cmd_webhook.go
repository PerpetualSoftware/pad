package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func webhooksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "Manage workspace webhooks",
		Long: `Manage webhooks that receive POST notifications when events occur.

Examples:
  pad webhook list
  pad webhook create https://httpbin.org/post --events "item.created,item.updated"
  pad webhook delete 7fde5e41
  pad webhook test 7fde5e41`,
	}

	cmd.AddCommand(
		webhooksListCmd(),
		webhooksCreateCmd(),
		webhooksDeleteCmd(),
		webhooksTestCmd(),
	)

	return cmd
}

func webhooksListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List all webhooks in the workspace",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			hooks, err := client.ListWebhooks(ws)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(hooks)
			}

			if len(hooks) == 0 {
				fmt.Println("No webhooks configured.")
				return nil
			}

			dim := color.New(color.Faint)
			green := color.New(color.FgGreen)
			red := color.New(color.FgRed)

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				dim.Sprint("ID"),
				dim.Sprint("URL"),
				dim.Sprint("EVENTS"),
				dim.Sprint("ACTIVE"),
				dim.Sprint("FAILURES"),
			)
			for _, h := range hooks {
				// Truncate ID to 8 chars for display
				shortID := h.ID
				if len(shortID) > 8 {
					shortID = shortID[:8]
				}

				// Truncate URL if very long
				displayURL := h.URL
				if len(displayURL) > 40 {
					displayURL = displayURL[:37] + "..."
				}

				// Format events
				events := h.Events
				if events == "" || events == `["*"]` || events == "*" {
					events = "*"
				}

				// Active indicator
				activeStr := red.Sprint("✗")
				if h.Active {
					activeStr = green.Sprint("✓")
				}

				// Failure count with color
				failStr := fmt.Sprintf("%d", h.FailureCount)
				if h.FailureCount > 0 {
					failStr = red.Sprintf("%d", h.FailureCount)
				}

				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					shortID, displayURL, events, activeStr, failStr,
				)
			}
			w.Flush()
			return nil
		},
	}
}

func webhooksCreateCmd() *cobra.Command {
	var (
		eventsFlag string
		secretFlag string
	)

	cmd := &cobra.Command{
		Use:   "create <url>",
		Short: "Register a new webhook",
		Long: `Register a new webhook URL to receive event notifications.

Examples:
  pad webhook create https://httpbin.org/post
  pad webhook create https://slack.com/webhook/... --events "item.created,item.updated"
  pad webhook create https://example.com/hook --secret "mysecret"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			input := models.WebhookCreate{
				URL:    args[0],
				Events: eventsFlag,
				Secret: secretFlag,
			}

			hook, err := client.CreateWebhook(ws, input)
			if err != nil {
				// TASK-788: emit structured marker so MCP stdio classifier
				// can surface ErrPlanLimitExceeded instead of ErrServerError.
				if apiErr, ok := err.(*cli.APIError); ok {
					if apiErr.AsPlanLimit() != nil {
						cli.WritePlanLimitError(os.Stderr, apiErr)
						return fmt.Errorf("webhook creation blocked: plan limit reached")
					}
				}
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(hook)
			}

			green := color.New(color.FgGreen)
			fmt.Printf("%s Webhook created\n", green.Sprint("✓"))
			fmt.Printf("  ID:     %s\n", hook.ID)
			fmt.Printf("  URL:    %s\n", hook.URL)
			events := hook.Events
			if events == "" {
				events = "*"
			}
			fmt.Printf("  Events: %s\n", events)
			return nil
		},
	}

	cmd.Flags().StringVar(&eventsFlag, "events", "", "comma-separated event types (default: all)")
	cmd.Flags().StringVar(&secretFlag, "secret", "", "shared secret for HMAC signature verification")

	return cmd
}

func webhooksDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <id>",
		Short:   "Delete a webhook",
		Aliases: []string{"rm"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			err := client.DeleteWebhook(ws, args[0])
			if err != nil {
				return err
			}

			green := color.New(color.FgGreen)
			fmt.Printf("%s Webhook %s deleted\n", green.Sprint("✓"), args[0])
			return nil
		},
	}
}

func webhooksTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test <id>",
		Short: "Send a test payload to a webhook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			err := client.TestWebhook(ws, args[0])
			if err != nil {
				return err
			}

			green := color.New(color.FgGreen)
			fmt.Printf("%s Test payload sent to webhook %s\n", green.Sprint("✓"), args[0])
			return nil
		},
	}
}

// --- bulk-update ---
