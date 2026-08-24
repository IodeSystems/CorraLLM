package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/iodesystems/corrallm/internal/config"
)

// liveConfig loads the configuration a command should reason about, and says
// where it came from.
//
// Since P26 the database IS the config and `~/.corrallm/config.yml` is a
// zero-byte leftover on a migrated machine. Commands that kept reading the file
// did not fail — they succeeded about nothing, which is worse: `corrallm tools`
// reported "no tools declared" on a box running three, and `validate` printed
// "ok — 0 models, 0 lanes" for the very config it exists to gate a restart on.
//
// The rule mirrors what `serve` does, with one addition for the linting case:
//
//   - an explicit --config naming a file with content in it wins, because
//     validating a file you named is a real thing to want;
//   - otherwise the database, which is what the daemon will actually boot from;
//   - the file only if the database holds nothing at all, which is the one-time
//     import case a fresh machine is in.
func liveConfig(configPath string) (*config.Config, string, error) {
	path := strings.TrimSpace(configPath)
	if path != "" && fileHasContent(path) {
		c, err := config.Load(path)
		return c, path, err
	}

	p := derivePaths(defaultHome(), "", "")
	db, src, err := openConfigDB(p.db)
	if err != nil {
		return nil, "", err
	}
	defer db.Close()

	c, err := src.Load(context.Background())
	if err != nil {
		return nil, "", err
	}
	if len(c.Servers) == 0 && len(c.AllModels()) == 0 && path != "" {
		// An empty database and a named file: this machine has not imported
		// yet, so the file is still the truth. Reporting "0 models" about the
		// database here would describe the wrong thing.
		fc, ferr := config.Load(path)
		if ferr == nil {
			return fc, path, nil
		}
	}
	return c, p.db, nil
}

// fileHasContent reports whether a path is a regular file with bytes in it. A
// zero-byte config is what a migrated machine leaves behind, and it is not a
// configuration.
func fileHasContent(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

// describeConfigSource renders a source for humans: a path, or the database.
func describeConfigSource(src string) string {
	if strings.HasSuffix(src, ".db") {
		return fmt.Sprintf("%s (database)", src)
	}
	return src
}
