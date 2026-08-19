package api

import (
	"context"
	"fmt"
	"time"

	"github.com/iodesystems/corrallm/internal/configdb"
)

// Configuration history for the dashboard.
//
// This matters more than it looks. Config used to be a file, so "what changed"
// was answerable with a text editor and a backup copy — badly, since the daemon
// rewrote the file and destroyed comments, but answerable. The file is gone
// now, which means this IS the undo. Leaving it CLI-only would have traded a
// bad answer for none.

// ConfigHistoryInput bounds the listing.
type ConfigHistoryInput struct {
	Limit int `query:"limit" doc:"Newest first. Default 20."`
}

// ConfigRevisionView is one recorded state of the configuration.
type ConfigRevisionView struct {
	ID int64  `json:"id"`
	At string `json:"at"`
	// Note is what caused it — "edited through the dashboard", "imported from
	// <path>", "restored revision 4". A date and a byte count alone is as
	// useful as no history.
	Note string `json:"note"`
	// Bytes is the size of the stored YAML. The body is fetched per revision:
	// a listing of fifty would otherwise be megabytes to render a list of dates.
	Bytes int `json:"bytes"`
	// Current marks the revision the running config came from, which is the
	// first thing anybody looks for in a list of revisions.
	Current bool `json:"current"`
}

// ConfigHistoryOutput is the revision list.
type ConfigHistoryOutput struct {
	Body struct {
		Revisions []ConfigRevisionView `json:"revisions"`
	}
}

// ConfigHistory lists recorded configuration revisions, newest first.
func (h *Handlers) ConfigHistory(ctx context.Context, in *ConfigHistoryInput) (*ConfigHistoryOutput, error) {
	out := &ConfigHistoryOutput{}
	out.Body.Revisions = []ConfigRevisionView{}
	if h.ConfigSource == nil {
		return out, nil
	}
	revs, err := configdb.Revisions(ctx, h.ConfigSource.DB, in.Limit)
	if err != nil {
		return nil, err
	}
	for i, r := range revs {
		out.Body.Revisions = append(out.Body.Revisions, ConfigRevisionView{
			ID:    r.ID,
			At:    r.At.Format(time.RFC3339),
			Note:  r.Note,
			Bytes: r.Size,
			// Newest first, so the head of the list is what is running — the
			// tables always hold what the last successful save wrote.
			Current: i == 0,
		})
	}
	return out, nil
}

// ConfigRevisionInput names one revision.
type ConfigRevisionInput struct {
	ID int64 `path:"id"`
}

// ConfigYAMLOutput carries a configuration as YAML.
type ConfigYAMLOutput struct {
	Body struct {
		YAML string `json:"yaml"`
	}
}

// ConfigRevision returns one revision's configuration.
func (h *Handlers) ConfigRevision(ctx context.Context, in *ConfigRevisionInput) (*ConfigYAMLOutput, error) {
	if h.ConfigSource == nil {
		return nil, fmt.Errorf("this daemon has no configuration store")
	}
	y, err := configdb.RevisionYAML(ctx, h.ConfigSource.DB, in.ID)
	if err != nil {
		return nil, err
	}
	out := &ConfigYAMLOutput{}
	out.Body.YAML = y
	return out, nil
}

// ConfigExportInput takes nothing; it exports what is stored right now.
type ConfigExportInput struct{}

// ConfigExport renders the CURRENT configuration as YAML.
//
// The escape hatch, on the dashboard rather than only in a terminal: SQLite is
// not readable in an editor, so this is how a backup or a review happens for
// somebody who is not going to ssh in.
func (h *Handlers) ConfigExport(ctx context.Context, _ *ConfigExportInput) (*ConfigYAMLOutput, error) {
	if h.ConfigSource == nil {
		return nil, fmt.Errorf("this daemon has no configuration store")
	}
	y, err := h.ConfigSource.ExportYAML(ctx)
	if err != nil {
		return nil, err
	}
	out := &ConfigYAMLOutput{}
	out.Body.YAML = string(y)
	return out, nil
}

// ConfigRestoreInput names the revision to make current.
type ConfigRestoreInput struct {
	Body struct {
		ID int64 `json:"id"`
	}
}

// ConfigRestoreOutput reports the result.
type ConfigRestoreOutput struct {
	Body struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
}

// ConfigRestore makes an earlier revision current.
//
// Validated like any other save, so a revision that is no longer valid — it
// names a server since removed — is refused rather than restored into a daemon
// that cannot run it. Recorded as a NEW revision rather than rewinding: history
// that can be rewritten is not history.
//
// Reloads afterwards, because a restore nobody applied is a restore that did
// not happen as far as the operator is concerned.
func (h *Handlers) ConfigRestore(ctx context.Context, in *ConfigRestoreInput) (*ConfigRestoreOutput, error) {
	if h.ConfigSource == nil {
		return nil, fmt.Errorf("this daemon has no configuration store")
	}
	if err := h.ConfigSource.Restore(ctx, in.Body.ID); err != nil {
		return nil, err
	}
	out := &ConfigRestoreOutput{}
	out.Body.OK = true
	out.Body.Message = fmt.Sprintf("restored revision %d", in.Body.ID)
	if h.Reload != nil {
		if err := h.Reload(); err != nil {
			out.Body.Message += "; the reload failed, so restart to pick it up"
		}
	}
	return out, nil
}
