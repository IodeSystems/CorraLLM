package api

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/store"
)

// The Warnings panel's kinds. The strings are the wire contract with the UI.
//
// Only queue-clash belongs to one model; the rest are sums over the box, so
// they are suppressed and tuned as a whole.
const (
	WarnQueueClash  = "queue-clash"
	WarnTurnedAway  = "turned-away"
	WarnUnanswered  = "unanswered"
	WarnSlowStretch = "slow-stretch"
	WarnOpenCallers = "open-callers"
)

// warningKinds says, per kind, whether it takes a subject (a served model) and
// whether it has a threshold to tune. A queue clash has none: the clash is
// arithmetic, and the tolerance for it is a longer maxWait, which is config.
var warningKinds = map[string]struct{ perModel, tunable bool }{
	WarnQueueClash:  {perModel: true},
	WarnTurnedAway:  {tunable: true},
	WarnUnanswered:  {tunable: true},
	WarnSlowStretch: {tunable: true},
	WarnOpenCallers: {tunable: true},
}

func checkWarningKind(kind, subject string) error {
	k, ok := warningKinds[kind]
	switch {
	case !ok:
		return huma.Error400BadRequest(fmt.Sprintf("unknown warning kind %q", kind))
	case k.perModel && subject == "":
		return huma.Error400BadRequest(kind + " belongs to one model: name it in subject")
	case !k.perModel && subject != "":
		return huma.Error400BadRequest(kind + " is box-wide: subject must be empty")
	}
	return nil
}

// WarningPrefView is one warning's standing orders.
type WarningPrefView struct {
	Kind              string  `json:"kind" doc:"queue-clash | turned-away | unanswered | slow-stretch | open-callers."`
	Subject           string  `json:"subject" doc:"Served model for a per-model kind; empty for a box-wide one."`
	Suppressed        bool    `json:"suppressed" doc:"Hidden from the Warnings panel right now."`
	SuppressedUntil   int64   `json:"suppressedUntilMs" doc:"When the suppression lapses, unix millis. 0 when not suppressed or suppressed forever."`
	SuppressedForever bool    `json:"suppressedForever" doc:"Suppressed with no end."`
	Threshold         float64 `json:"threshold" doc:"What the warning must reach before it is raised. 0 = the built-in default."`
}

// WarningPrefsOutput lists every warning with a standing order in force.
type WarningPrefsOutput struct {
	Body struct {
		Prefs []WarningPrefView `json:"prefs"`
		// The default the slow-stretch threshold replaces, so the UI can say
		// what it is raising FROM without a copy of the constant.
		SlowStretchDefault float64 `json:"slowStretchDefault" doc:"The built-in slow-stretch multiple of the mean."`
	}
}

// WarningPrefs lists suppressions still in force and thresholds that are set.
// A lapsed suppression is left out rather than reported as false, so the list
// is exactly what is changing the panel.
func (h *Handlers) WarningPrefs(_ context.Context, _ *struct{}) (*WarningPrefsOutput, error) {
	out := &WarningPrefsOutput{}
	out.Body.Prefs = []WarningPrefView{}
	out.Body.SlowStretchDefault = store.SlowSpellMultiple
	if h.Store == nil {
		return out, nil
	}
	rows, err := h.Store.WarningPrefs()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	for _, p := range rows {
		v := WarningPrefView{Kind: p.Kind, Subject: p.Subject, Threshold: p.Threshold}
		if p.SuppressedAt(now) {
			v.Suppressed = true
			v.SuppressedForever = p.SuppressUntilMS == store.SuppressForever
			if !v.SuppressedForever {
				v.SuppressedUntil = p.SuppressUntilMS
			}
		}
		if !v.Suppressed && v.Threshold == 0 {
			continue
		}
		out.Body.Prefs = append(out.Body.Prefs, v)
	}
	return out, nil
}

// SuppressWarningInput hides one warning for a while, for good, or lifts that.
type SuppressWarningInput struct {
	Body struct {
		Kind    string `json:"kind"`
		Subject string `json:"subject,omitempty" doc:"Served model, for a per-model kind."`
		Minutes int64  `json:"minutes,omitempty" doc:"How long to hide it. 0 with forever unset uses the default of one day."`
		Forever bool   `json:"forever,omitempty" doc:"Hide it with no end."`
		Lift    bool   `json:"lift,omitempty" doc:"Show it again now. Overrides minutes and forever."`
	}
}

// suppressDefaultMinutes is one day: long enough to get past the rest of the
// working day, short enough that a problem nobody fixed comes back on its own.
const suppressDefaultMinutes = 24 * 60

// SuppressWarning writes a suppression. It changes what the panel says, never
// what the box does.
func (h *Handlers) SuppressWarning(_ context.Context, in *SuppressWarningInput) (*ConfigMutationOutput, error) {
	b := in.Body
	if err := checkWarningKind(b.Kind, b.Subject); err != nil {
		return nil, err
	}
	if b.Minutes < 0 {
		return nil, huma.Error400BadRequest("minutes cannot be negative")
	}
	if h.Store == nil {
		return nil, huma.Error503ServiceUnavailable("no store configured")
	}
	name := b.Kind
	if b.Subject != "" {
		name += " for " + b.Subject
	}
	var until int64
	var what string
	switch {
	case b.Lift:
		what = name + " shown again"
	case b.Forever:
		until = store.SuppressForever
		what = name + " hidden until shown again"
	default:
		m := b.Minutes
		if m == 0 {
			m = suppressDefaultMinutes
		}
		t := time.Now().Add(time.Duration(m) * time.Minute)
		until = t.UnixMilli()
		what = fmt.Sprintf("%s hidden until %s", name, t.Format("2006-01-02 15:04"))
	}
	if err := h.Store.SetWarningSuppress(b.Kind, b.Subject, until); err != nil {
		return nil, err
	}
	out := &ConfigMutationOutput{}
	out.Body.OK = true
	out.Body.Message = what
	return out, nil
}

// SetWarningThresholdInput sets how far a box-wide warning must go before it is
// raised.
type SetWarningThresholdInput struct {
	Body struct {
		Kind      string  `json:"kind"`
		Threshold float64 `json:"threshold" doc:"turned-away / unanswered / open-callers: raise it only above this many. slow-stretch: the multiple of the mean a request must pass. 0 returns to the default."`
	}
}

// SetWarningThreshold writes a tolerance. Like a suppression, it changes what
// the panel says and nothing the box does.
func (h *Handlers) SetWarningThreshold(_ context.Context, in *SetWarningThresholdInput) (*ConfigMutationOutput, error) {
	b := in.Body
	if err := checkWarningKind(b.Kind, ""); err != nil {
		return nil, err
	}
	if !warningKinds[b.Kind].tunable {
		return nil, huma.Error400BadRequest(b.Kind + " has no threshold")
	}
	if b.Threshold < 0 {
		return nil, huma.Error400BadRequest("threshold cannot be negative")
	}
	// A multiple at or under 1 flags every request slower than the mean — half
	// of them — which is the panel crying wolf, not a tolerance.
	if b.Kind == WarnSlowStretch && b.Threshold != 0 && b.Threshold <= 1 {
		return nil, huma.Error400BadRequest("slow-stretch multiple must be above 1")
	}
	if h.Store == nil {
		return nil, huma.Error503ServiceUnavailable("no store configured")
	}
	if err := h.Store.SetWarningThreshold(b.Kind, "", b.Threshold); err != nil {
		return nil, err
	}
	out := &ConfigMutationOutput{}
	out.Body.OK = true
	out.Body.Message = fmt.Sprintf("%s threshold set to %g", b.Kind, b.Threshold)
	if b.Threshold == 0 {
		out.Body.Message = b.Kind + " threshold back to the default"
	}
	return out, nil
}

// SetModelMaxWaitInput sets one model's longest queue wait.
type SetModelMaxWaitInput struct {
	Name string `path:"name"`
	Body struct {
		MaxWait string `json:"maxWait" doc:"A Go duration, e.g. \"45s\". Empty clears the override and returns the model to the box-wide wait."`
	}
}

// SetModelMaxWait gives one model its own longest queue wait, or clears it.
//
// The other half of the queue-clash fix. SetModelQueueDepth lowers the depth to
// what the wait allows; this raises the wait to what the depth needs. The clash
// is one model's arithmetic, so the wait it writes is that model's too — the
// box-wide wait, and every other model's "come back", are untouched.
func (h *Handlers) SetModelMaxWait(ctx context.Context, in *SetModelMaxWaitInput) (*ConfigMutationOutput, error) {
	if in.Body.MaxWait != "" {
		d, err := time.ParseDuration(in.Body.MaxWait)
		if err != nil {
			return nil, huma.Error400BadRequest("maxWait is not a duration: " + err.Error())
		}
		if d <= 0 {
			return nil, huma.Error400BadRequest("maxWait must be positive")
		}
	}
	what := fmt.Sprintf("longest wait for model %s set to %s", in.Name, in.Body.MaxWait)
	if in.Body.MaxWait == "" {
		what = "longest wait for model " + in.Name + " cleared — back to the box-wide wait"
	}
	err := h.mutateConfig(ctx, what, func(c *config.Config) error {
		return editAuthoredModel(c, in.Name, func(m *config.Model) { setModelMaxWait(m, in.Body.MaxWait) })
	})
	if err != nil {
		return nil, err
	}
	out := &ConfigMutationOutput{}
	out.Body.OK = true
	out.Body.Message = what
	return out, nil
}

// setModelMaxWait mirrors SetModelQueueDepth's rules: clearing the wait keeps a
// depth override set beside it, and each call builds a fresh struct so the
// authored and resolved copies never share a pointer.
func setModelMaxWait(m *config.Model, maxWait string) {
	sc := config.SchedulerConfig{MaxWait: maxWait}
	if m.Scheduler != nil {
		sc.MaxQueueDepth = m.Scheduler.MaxQueueDepth
	}
	if sc.MaxWait == "" && sc.MaxQueueDepth == 0 {
		m.Scheduler = nil
		return
	}
	m.Scheduler = &sc
}

// waitForDepthMS is the maxWait at which a queue of depth becomes reachable:
// the inverse of reachable depth, depth × meanService / capacity. The other way
// out of a queue clash. Whole seconds, rounded up, so the value written is one
// a person would type and still clears the bound. 0 if not computable.
func waitForDepthMS(depth, capacity int, meanServiceMS float64) int64 {
	if depth <= 0 || capacity <= 0 || meanServiceMS <= 0 {
		return 0
	}
	ms := float64(depth) * meanServiceMS / float64(capacity)
	return int64(math.Ceil(ms/1000)) * 1000
}
