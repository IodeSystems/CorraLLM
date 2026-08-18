package api

import (
	"context"
	"time"

	"github.com/iodesystems/corrallm/internal/store"
)

// BuildHistoryStore adapts the store to the narrow interface the Builder wants.
//
// The adapter exists so internal/toolchain never imports internal/store: a
// build is a process and a log, and where the record goes is not its business.
// It also keeps the store's signature free to grow — Recent and the log lookup
// are read by the API directly and have no reason to be on the Builder's
// interface.
type BuildHistoryStore struct{ S *store.Store }

func (b BuildHistoryStore) Start(ctx context.Context, tool, host string, at time.Time) (int64, error) {
	return b.S.StartToolBuild(ctx, tool, host, at)
}

func (b BuildHistoryStore) Finish(ctx context.Context, id int64, status string, finishedAt time.Time,
	skipped bool, version, stamp, errMsg, log string) error {
	return b.S.FinishToolBuild(ctx, id, store.ToolBuild{
		Status: status, FinishedAt: finishedAt, Skipped: skipped,
		Version: version, Stamp: stamp, Error: errMsg, Log: log,
	})
}
