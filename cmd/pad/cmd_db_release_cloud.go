package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// openServerDatabase opens the database `pad server start` on this host would
// open: PostgreSQL when PAD_DB_DRIVER=postgres, else the configured SQLite
// file. Opening applies pending migrations, as the server's boot would.
func openServerDatabase() (*store.Store, string, error) {
	if os.Getenv("PAD_DB_DRIVER") == "postgres" {
		pgURL := os.Getenv("PAD_DATABASE_URL")
		if pgURL == "" {
			return nil, "", fmt.Errorf("PAD_DATABASE_URL is required when PAD_DB_DRIVER=postgres")
		}
		s, err := store.NewPostgres(pgURL)
		if err != nil {
			return nil, "", fmt.Errorf("open postgres: %w", err)
		}
		return s, "postgres database " + pgDbnameFromURL(pgURL), nil
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, "", fmt.Errorf("load config: %w", err)
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		return nil, "", fmt.Errorf("SQLite database not found: %s", cfg.DBPath)
	}
	s, err := store.New(cfg.DBPath)
	if err != nil {
		return nil, "", fmt.Errorf("open database: %w", err)
	}
	return s, cfg.DBPath, nil
}

// dbReleaseCloudCmd is the deliberate way to take a Pad Cloud database to a
// self-hosted instance (TASK-3551). A cloud boot marks its database
// cloud-owned, and a non-cloud boot then refuses to convert free users to
// self-hosted (unlimited); this records that the move is intended. It runs on
// the server host against the database directly, with no API route, like
// `pad auth reset-password`.
func dbReleaseCloudCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "release-cloud",
		Short: "Let a self-hosted boot convert this database's free users to self-hosted (moving off Pad Cloud)",
		Long: `Records that this database is deliberately leaving Pad Cloud, so the next
non-cloud boot converts free and empty plans to self-hosted (no limits).

A database a cloud-mode server has booted is marked cloud-owned, and a
non-cloud boot leaves its plans alone (TASK-3551). Run this on the server
host, against the database the server uses. It refuses while users carry
Pad Cloud billing state (a Stripe plan source or customer id) unless
--force. A later cloud-mode boot marks the database cloud-owned again.`,
		Example: `  pad db release-cloud
  pad db release-cloud --force`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, where, err := openServerDatabase()
			if err != nil {
				return err
			}
			defer s.Close()
			o, err := s.ReleaseCloudOwnership(force)
			if errors.Is(err, store.ErrCloudFingerprintPresent) {
				return fmt.Errorf("%d user(s) in %s carry Pad Cloud billing state (a Stripe plan source or customer id); "+
					"releasing would convert the free ones to self-hosted (unlimited). Re-run with --force if that is intended", o.StripeUsers, where)
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Released %s from Pad Cloud ownership (was: %q).\n", where, o.Marker)
			if o.StripeUsers > 0 {
				fmt.Fprintf(out, "%d user(s) still carry Pad Cloud billing state.\n", o.StripeUsers)
			}
			fmt.Fprintln(out, "The next non-cloud boot converts free and empty plans to self-hosted.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "release even though users carry Pad Cloud billing state")
	return cmd
}
