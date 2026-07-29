package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/config"
)

// DefaultConfigPath is where corrallm keeps the config it OWNS.
//
// A per-user path rather than the working directory, because a managed config
// is not a project file: it follows the daemon, not the checkout it happened to
// be started from.
func DefaultConfigPath() string {
	if p := os.Getenv("CORRALLM_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./corrallm.yaml"
	}
	return filepath.Join(home, ".corrallm", "config.yml")
}

// config import converts a hand-written config into the managed one, carrying
// its comments across as notes.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect and migrate corrallm's own configuration"}

	var out string
	var force bool
	imp := &cobra.Command{
		Use:   "import <hand-written.yaml>",
		Short: "Convert a hand-written config into the managed one, keeping comments as notes",
		Long: "Convert a hand-written config into the managed one.\n\n" +
			"A managed config is rewritten by corrallm whenever configuration changes, and a\n" +
			"marshaller cannot keep YAML comments. This lifts the comments above each model\n" +
			"and server into that entry's `notes` field first, so the reasoning survives the\n" +
			"migration and shows up in the dashboard beside what it describes.\n\n" +
			"Comments that belong to no single entry — file headers, section banners — cannot\n" +
			"be attached to anything and are REPORTED rather than dropped silently, so you can\n" +
			"decide where they go.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			c, err := config.Load(args[0])
			if err != nil {
				return fmt.Errorf("the source config does not load; fix it before importing: %w", err)
			}
			orphaned, err := config.ImportComments(src, c)
			if err != nil {
				return err
			}
			dst := pick(out, DefaultConfigPath())
			if _, err := os.Stat(dst); err == nil && !force {
				return fmt.Errorf("%s already exists; pass --force to overwrite", dst)
			}
			if err := config.SaveValidated(dst, c); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "wrote %s — %d models, %d servers, %d lanes\n",
				dst, len(c.Models), len(c.Servers), len(c.Lanes))
			if len(orphaned) > 0 {
				fmt.Fprintf(w, "\n%d comment block(s) belonged to no single entry and were NOT carried over.\n", len(orphaned))
				fmt.Fprintf(w, "They are printed below so nothing is lost silently — move anything worth keeping\ninto a notes field or your docs:\n\n")
				for _, o := range orphaned {
					fmt.Fprintf(w, "--- %s\n", o)
				}
			}
			return nil
		},
	}
	imp.Flags().StringVar(&out, "out", "", "destination (default "+DefaultConfigPath()+")")
	imp.Flags().BoolVar(&force, "force", false, "overwrite an existing managed config")

	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print the managed config path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), DefaultConfigPath())
			return nil
		},
	}
	cmd.AddCommand(imp, pathCmd)
	return cmd
}
