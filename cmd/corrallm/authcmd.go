package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/configdb"
)

// The auth subcommands. They write the config database directly, so they work
// when the dashboard cannot be reached — which is the case they exist for: SSO
// broke after the admin token was disabled, and the API has no way in left.
func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Dashboard sign-in settings, edited without the API"}
	tok := &cobra.Command{
		Use:   "admin-token",
		Short: "Enable, disable or show whether /api accepts the admin token",
		Long: "The admin token (admin.token in corrallm's home) is accepted by /api unless\n" +
			"auth.adminToken is disabled. Disabling it from the dashboard needs a working SSO\n" +
			"admin session; this command is the way back when SSO stops working.\n\n" +
			"A running daemon picks the change up on `corrallm service reload`.",
	}
	var dbPath string
	tok.PersistentFlags().StringVar(&dbPath, "db", "", "database holding the config")
	for _, v := range []string{config.AdminTokenEnabled, config.AdminTokenDisabled} {
		want := v
		verb := map[string]string{config.AdminTokenEnabled: "enable", config.AdminTokenDisabled: "disable"}[v]
		tok.AddCommand(&cobra.Command{
			Use:   verb,
			Short: verb + " the admin token",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				db, src, err := openConfigDB(derivePaths(defaultHome(), "", dbPath).db)
				if err != nil {
					return err
				}
				defer db.Close()
				return setAdminToken(cmd.Context(), src, want, cmd.OutOrStdout())
			},
		})
	}
	tok.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Print whether the admin token is accepted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, _, err := openConfigDB(derivePaths(defaultHome(), "", dbPath).db)
			if err != nil {
				return err
			}
			defer db.Close()
			return adminTokenStatus(cmd.Context(), db, cmd.OutOrStdout())
		},
	})
	cmd.AddCommand(tok)
	return cmd
}

// setAdminToken stores auth.adminToken through the validated update path.
func setAdminToken(ctx context.Context, src *configdb.Source, v string, out io.Writer) error {
	_, err := src.WithNote("admin token "+v+" from the command line").Update(ctx, func(c *config.Config) error {
		a := config.Auth{}
		if c.Auth != nil {
			a = *c.Auth
		}
		a.AdminToken = v
		if v == config.AdminTokenEnabled {
			a.AdminToken = "" // the default; absent reads the same
		}
		c.Auth = &a
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "admin token %s — `corrallm service reload` to apply it to a running daemon\n", v)
	return nil
}

func adminTokenStatus(ctx context.Context, db *sql.DB, out io.Writer) error {
	c, err := configdb.Read(ctx, db)
	if err != nil {
		return err
	}
	if c.Auth.AdminTokenOn() {
		fmt.Fprintln(out, config.AdminTokenEnabled)
	} else {
		fmt.Fprintln(out, config.AdminTokenDisabled)
	}
	return nil
}
