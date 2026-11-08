package api

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/corrallm/internal/proxy"
)

// spendTokenDefaultDays is how long a cost token lasts when nothing is asked for.
const spendTokenDefaultDays = 90

// SpendReportOutput is the same report a cost token reads.
type SpendReportOutput struct {
	Body proxy.SpendReport
}

// SpendReport reports the month so far.
func (h *Handlers) SpendReport(_ context.Context, _ *struct{}) (*SpendReportOutput, error) {
	if h.Proxy == nil {
		return nil, huma.Error503ServiceUnavailable("proxy unavailable")
	}
	rep, err := h.Proxy.Spend(time.Now())
	if err != nil {
		return nil, err
	}
	return &SpendReportOutput{Body: rep}, nil
}

// SpendReconcileInput is the real figure: what was spent, or what is left.
type SpendReconcileInput struct {
	Body struct {
		USD  *float64 `json:"usd,omitempty" minimum:"0" doc:"What the provider's console shows spent this period, in USD."`
		Left *float64 `json:"left,omitempty" minimum:"0" doc:"What the provider's console shows left of the credit, in USD."`
	}
}

// SpendReconcile stores the real figure and returns the report with it.
func (h *Handlers) SpendReconcile(_ context.Context, in *SpendReconcileInput) (*SpendReportOutput, error) {
	if h.Proxy == nil {
		return nil, huma.Error503ServiceUnavailable("proxy unavailable")
	}
	var usd float64
	switch b := in.Body; {
	case (b.USD == nil) == (b.Left == nil):
		return nil, huma.Error400BadRequest("give usd or left")
	case b.USD != nil:
		usd = *b.USD
	default:
		var err error
		if usd, err = proxy.SpentFromLeft(h.Proxy.Config(), *b.Left); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
	}
	now := time.Now()
	if err := h.Proxy.ReconcileSpend(usd, now); err != nil {
		return nil, err
	}
	rep, err := h.Proxy.Spend(now)
	if err != nil {
		return nil, err
	}
	return &SpendReportOutput{Body: rep}, nil
}

// SpendBudgetInput adds, changes or removes a timed budget on a provider.
type SpendBudgetInput struct {
	Body struct {
		Action   string   `json:"action" enum:"add,update,delete"`
		ID       int64    `json:"id,omitempty" doc:"The budget, for update and delete."`
		Provider string   `json:"provider,omitempty" doc:"The provider, for add."`
		USD      *float64 `json:"usd,omitempty" doc:"The amount in USD."`
		Left     *float64 `json:"left,omitempty" doc:"Set the amount so that this much USD remains."`
		TTL      string   `json:"ttl,omitempty" doc:"Time from now until it expires, e.g. 72h."`
		Expires  string   `json:"expires,omitempty" doc:"When it expires, RFC 3339."`
		Note     *string  `json:"note,omitempty"`
		Banked   *bool    `json:"banked,omitempty" doc:"Spent after the provider's renewing chunks (calendar day), not before them. Default false."`
	}
}

// SpendBudget changes a timed budget and returns the report with it.
func (h *Handlers) SpendBudget(_ context.Context, in *SpendBudgetInput) (*SpendReportOutput, error) {
	if h.Proxy == nil {
		return nil, huma.Error503ServiceUnavailable("proxy unavailable")
	}
	b, now := in.Body, time.Now()
	var err error
	switch b.Action {
	case "add":
		note, usd := "", 0.0
		if b.Note != nil {
			note = *b.Note
		}
		if b.USD != nil {
			usd = *b.USD
		}
		err = h.Proxy.AddBudget(b.Provider, usd, b.TTL, b.Expires, note, b.Banked != nil && *b.Banked, now)
	case "update":
		err = h.Proxy.EditBudget(b.ID, proxy.BudgetEdit{USD: b.USD, Left: b.Left, TTL: b.TTL, Expires: b.Expires, Note: b.Note, Banked: b.Banked}, now)
	case "delete":
		err = h.Proxy.RemoveBudget(b.ID)
	}
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	rep, err := h.Proxy.Spend(now)
	if err != nil {
		return nil, err
	}
	return &SpendReportOutput{Body: rep}, nil
}

// SpendTokenView is a cost token as listed.
type SpendTokenView struct {
	ID        int64  `json:"id"`
	Label     string `json:"label"`
	CreatedMS int64  `json:"createdMs" doc:"Unix milliseconds."`
	ExpiresMS int64  `json:"expiresMs" doc:"Unix milliseconds; 0 never expires."`
	RevokedMS int64  `json:"revokedMs" doc:"Unix milliseconds; 0 is live."`
	UsedMS    int64  `json:"usedMs" doc:"Unix milliseconds of the last read; 0 never used."`
}

// SpendTokensOutput lists cost tokens.
type SpendTokensOutput struct {
	Body struct {
		Tokens []SpendTokenView `json:"tokens"`
	}
}

// SpendTokens lists cost tokens.
func (h *Handlers) SpendTokens(_ context.Context, _ *struct{}) (*SpendTokensOutput, error) {
	out := &SpendTokensOutput{}
	out.Body.Tokens = []SpendTokenView{}
	if h.Store == nil {
		return out, nil
	}
	rows, err := h.Store.SpendTokens()
	if err != nil {
		return nil, err
	}
	for _, t := range rows {
		out.Body.Tokens = append(out.Body.Tokens, SpendTokenView{ID: t.ID, Label: t.Label,
			CreatedMS: t.CreatedMS, ExpiresMS: t.ExpiresMS, RevokedMS: t.RevokedMS, UsedMS: t.UsedMS})
	}
	return out, nil
}

// SpendTokenCreateInput names a new token.
type SpendTokenCreateInput struct {
	Body struct {
		Label string `json:"label,omitempty" doc:"What the token is for."`
		Days  int    `json:"days,omitempty" doc:"Days until it expires. 0 is 90; negative never expires."`
	}
}

// SpendTokenCreateOutput carries the token, the only time it is shown.
type SpendTokenCreateOutput struct {
	Body struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
}

// SpendTokenCreate makes a cost token.
func (h *Handlers) SpendTokenCreate(_ context.Context, in *SpendTokenCreateInput) (*SpendTokenCreateOutput, error) {
	if h.Proxy == nil {
		return nil, huma.Error503ServiceUnavailable("proxy unavailable")
	}
	days := in.Body.Days
	if days == 0 {
		days = spendTokenDefaultDays
	}
	token, id, err := h.Proxy.CreateSpendToken(in.Body.Label, time.Duration(max(days, 0))*24*time.Hour, time.Now())
	if err != nil {
		return nil, err
	}
	out := &SpendTokenCreateOutput{}
	out.Body.ID, out.Body.Token = id, token
	return out, nil
}

// SpendTokenRevokeInput names a token.
type SpendTokenRevokeInput struct {
	Body struct {
		ID int64 `json:"id"`
	}
}

// SpendTokenRevokeOutput says whether a live token ended.
type SpendTokenRevokeOutput struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

// SpendTokenRevoke ends a cost token.
func (h *Handlers) SpendTokenRevoke(_ context.Context, in *SpendTokenRevokeInput) (*SpendTokenRevokeOutput, error) {
	if h.Store == nil {
		return nil, huma.Error503ServiceUnavailable("store unavailable")
	}
	ok, err := h.Store.RevokeSpendToken(in.Body.ID, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, huma.Error404NotFound("no live cost token with that id")
	}
	out := &SpendTokenRevokeOutput{}
	out.Body.OK = true
	return out, nil
}
