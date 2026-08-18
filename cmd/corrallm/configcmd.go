package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/configdb"

	_ "modernc.org/sqlite"
)

// openConfigDB opens the database configuration now lives in.
//
// Read-write on purpose even for export: the schema is applied on open, and a
// brand-new database that cannot be initialised would fail with something far
// less obvious than "cannot create tables".
func openConfigDB(dbPath string) (*sql.DB, *configdb.Source, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, nil, err
	}
	if err := configdb.Apply(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return db, &configdb.Source{DB: db}, nil
}

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
	var dbPath string
	var exportOut string
	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Write the stored configuration out as YAML",
		Long: "Renders the configuration held in the database as YAML.\n\n" +
			"This is the escape hatch. Config lives in SQLite, which is not readable with a\n" +
			"text editor and not diffable in git; an export is how you take a backup, review\n" +
			"a change, or move a config to another machine. It exports what was STORED rather\n" +
			"than what was resolved, so the output can be imported again — an export you\n" +
			"cannot re-import is a backup of nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := derivePaths(defaultHome(), "", dbPath)
			db, src, err := openConfigDB(p.db)
			if err != nil {
				return err
			}
			defer db.Close()
			out, err := src.ExportYAML(cmd.Context())
			if err != nil {
				return err
			}
			if exportOut == "" || exportOut == "-" {
				_, err = cmd.OutOrStdout().Write(out)
				return err
			}
			if err := os.WriteFile(exportOut, out, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (%d bytes)\n", exportOut, len(out))
			return nil
		},
	}
	exportCmd.Flags().StringVar(&exportOut, "out", "", "write here instead of stdout")
	exportCmd.Flags().StringVar(&dbPath, "db", "", "database holding the config")

	var loadDB string
	var loadForce bool
	loadCmd := &cobra.Command{
		Use:   "load <config.yaml>",
		Short: "Replace the stored configuration with a YAML file",
		Long: "Parses, validates and stores a YAML config, REPLACING what is there.\n\n" +
			"The inverse of export, and the way to restore a backup or move a config between\n" +
			"machines. Validation happens before anything is written: an invalid config is\n" +
			"refused rather than stored and discovered at the next restart.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := derivePaths(defaultHome(), "", loadDB)
			db, src, err := openConfigDB(p.db)
			if err != nil {
				return err
			}
			defer db.Close()
			empty, err := configdb.IsEmpty(cmd.Context(), db)
			if err != nil {
				return err
			}
			if !empty && !loadForce {
				return fmt.Errorf("the database already holds a configuration; pass --force to replace it (export it first if you want a copy)")
			}
			c, err := src.ImportFile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stored %s — %d models, %d servers, %d lanes\n",
				args[0], len(c.AllModels()), len(c.Servers), len(c.Lanes))
			return nil
		},
	}
	loadCmd.Flags().StringVar(&loadDB, "db", "", "database to store the config in")
	loadCmd.Flags().BoolVar(&loadForce, "force", false, "replace an existing stored configuration")

	cmd.AddCommand(imp, pathCmd, exportCmd, loadCmd)
	return cmd
}
