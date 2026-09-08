package proc

import "context"

// Why a backend loaded, recorded for somebody to read later.
//
// The activity log carried `load_ms` — the time a request spent waiting for a
// spawn — and nothing about the circumstances. Diagnosing the 2026-09-05
// slowdown therefore took a database session: the numbers showed one model
// being reloaded over and over, and no screen said what kept displacing it or
// who kept asking for it. Those two facts live here.
//
// Optional. A Manager with no recorder behaves exactly as before, which keeps
// every test and the agent path unchanged.

// ModelLoadEvent is one backend coming up, and the circumstances.
type ModelLoadEvent struct {
	Model    string
	Server   string
	// Requester is the caller whose request triggered the spawn, empty for a
	// preload at boot. It is what turns "this reloads constantly" into "this
	// reloads constantly because of that caller".
	Requester string
	// Evicted is what had to be unloaded to fit it. The other half of the same
	// question: a model that keeps loading is only half a story until you know
	// what keeps pushing it out.
	Evicted []string
	MS      int64
	OK      bool
	Err     string
}

// LoadRecorder is given each completed load. Implementations must not block:
// this runs on the load path, and a slow recorder would add latency to the
// spawn it is only describing.
type LoadRecorder interface {
	RecordLoad(ModelLoadEvent)
}

// SetLoadRecorder attaches one. nil disables recording.
func (m *Manager) SetLoadRecorder(r LoadRecorder) { m.loadRecorder = r }

func (m *Manager) recordLoad(ev ModelLoadEvent) {
	if m.loadRecorder == nil {
		return
	}
	m.loadRecorder.RecordLoad(ev)
}

type requesterKey struct{}

// WithRequester marks a context with the caller a spawn is being done for.
//
// Passed through the context rather than added to EnsureReady's signature
// because it is descriptive, not operative: nothing about the load DEPENDS on
// who asked, and threading an argument through every caller — including the
// agent and the preloader, which have no requester — to carry a note would put
// the note in the type system's way.
func WithRequester(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, requesterKey{}, key)
}

func requesterFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(requesterKey{}).(string)
	return s
}
