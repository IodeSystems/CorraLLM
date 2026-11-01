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

// SpendReconcileInput is the real figure.
type SpendReconcileInput struct {
	Body struct {
		USD float64 `json:"usd" minimum:"0" doc:"What the provider's console shows this month, in USD."`
	}
}

// SpendReconcile stores the real figure and returns the report with it.
func (h *Handlers) SpendReconcile(_ context.Context, in *SpendReconcileInput) (*SpendReportOutput, error) {
	if h.Proxy == nil {
		return nil, huma.Error503ServiceUnavailable("proxy unavailable")
	}
	now := time.Now()
	if err := h.Proxy.ReconcileSpend(in.Body.USD, now); err != nil {
		return nil, err
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
