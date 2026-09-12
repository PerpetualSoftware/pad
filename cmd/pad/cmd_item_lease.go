package main

import (
	"fmt"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// `pad item claim` / `pad item release` (#1221): the execution lease. A
// claim answers "may I be the one executing this right now" atomically —
// two pollers that both read "unclaimed" get one winner and one
// structured refusal naming the live holder, instead of two winners.

func itemClaimCmd() *cobra.Command {
	var (
		holderFlag string
		ttlFlag    string
	)

	cmd := &cobra.Command{
		Use:   "claim <ref>",
		Short: "Atomically claim an item for execution (lease with expiry)",
		Long: `Acquire the execution lease on an item, or refresh it if you already
hold it (a re-claim by the live holder extends the expiry — heartbeat).

Fails with the live holder and expiry when someone else holds the item, so
the loser can log who won and skip instead of double-working. The lease
expires on its own; a crashed holder blocks nobody past the TTL.

Examples:
  pad item claim TASK-5
  pad item claim TASK-5 --holder sweep-runner --ttl 30m`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ttlSeconds := 0
			if ttlFlag != "" {
				d, err := time.ParseDuration(ttlFlag)
				if err != nil {
					return fmt.Errorf("invalid --ttl %q: %w (use Go durations, e.g. 15m, 1h)", ttlFlag, err)
				}
				if d <= 0 {
					return fmt.Errorf("--ttl must be positive, got %s", d)
				}
				ttlSeconds = int(d.Seconds())
			}

			client, _ := getClient()
			ws := getWorkspace()

			lease, err := client.ClaimItem(ws, args[0], holderFlag, ttlSeconds)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(lease)
			}

			green := color.New(color.FgGreen)
			fmt.Printf("%s Lease acquired on %s\n", green.Sprint("✓"), args[0])
			fmt.Printf("  Holder:  %s\n", lease.Holder)
			fmt.Printf("  Expires: %s (%s)\n",
				lease.ExpiresAt.Local().Format("2006-01-02 15:04:05"),
				cli.LeaseCountdown(lease.ExpiresAt))
			return nil
		},
	}

	cmd.Flags().StringVar(&holderFlag, "holder", "", "lease holder identity (default: the authenticated user)")
	cmd.Flags().StringVar(&ttlFlag, "ttl", "", "lease duration, e.g. 15m, 1h (default: 15m; max 24h)")

	return cmd
}

func itemReleaseCmd() *cobra.Command {
	var holderFlag string

	cmd := &cobra.Command{
		Use:   "release <ref>",
		Short: "Release an item's execution lease (idempotent)",
		Long: `Release the execution lease you hold on an item. Releasing an absent or
already-expired lease is a no-op, not an error — cleanup code never needs
to check whether it still holds the lease first. Releasing another
holder's LIVE lease is refused.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			released, err := client.ReleaseItem(ws, args[0], holderFlag)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{"ref": args[0], "released": released})
			}

			green := color.New(color.FgGreen)
			if released {
				fmt.Printf("%s Lease released on %s\n", green.Sprint("✓"), args[0])
			} else {
				fmt.Printf("%s No live lease to release on %s (already expired or never held)\n", green.Sprint("✓"), args[0])
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&holderFlag, "holder", "", "lease holder identity (default: the authenticated user)")

	return cmd
}
