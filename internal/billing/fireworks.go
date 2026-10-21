package billing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fireworks reads rated costs from Fireworks' usageCosts:query API
// (https://docs.fireworks.ai/api-reference/query-usage-costs). The request key
// is whatever the credential presents; SCOPE SELF returns the costs of the
// authenticated principal, which is what a per-key budget is compared with.
type Fireworks struct {
	// Host is the API host that marks a provider as Fireworks.
	Host string
	// BaseURL is where the billing API lives. Tests point it at a fake.
	BaseURL string
	Client  *http.Client

	mu       sync.Mutex
	accounts map[string][]string // credential fingerprint -> account ids, cached: they do not change
}

// NewFireworks returns the production client.
func NewFireworks() *Fireworks {
	return &Fireworks{
		Host:    "api.fireworks.ai",
		BaseURL: "https://api.fireworks.ai",
		Client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (f *Fireworks) Name() string { return "fireworks" }

// Matches recognises a provider by its endpoint host.
func (f *Fireworks) Matches(u *url.URL) bool {
	return u != nil && strings.EqualFold(u.Hostname(), f.Host)
}

// maxWindow is the longest span one usageCosts call accepts.
const maxWindow = 31 * 24 * time.Hour

// maxPages bounds pagination so a server that never stops returning a token
// cannot hold the job forever.
const maxPages = 50

// Costs returns billed USD per hour and upstream model in [from, to).
func (f *Fireworks) Costs(ctx context.Context, headers map[string]string, from, to time.Time) (Result, error) {
	if to.Sub(from) > maxWindow {
		return Result{}, fmt.Errorf("fireworks: window %s exceeds the 31-day limit", to.Sub(from).Round(time.Hour))
	}
	ids, err := f.accountIDs(ctx, headers)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, id := range ids {
		pageToken := ""
		for page := 0; ; page++ {
			if page >= maxPages {
				return Result{}, fmt.Errorf("fireworks: usageCosts still paginating after %d pages", maxPages)
			}
			body := map[string]any{
				"startTime": from.UTC().Format(time.RFC3339),
				"endTime":   to.UTC().Format(time.RFC3339),
				"groupBy":   []string{"HOUR", "MODEL"},
				"scope":     "SELF",
			}
			if pageToken != "" {
				body["pageToken"] = pageToken
			}
			var out usageCostsResponse
			if err := f.do(ctx, http.MethodPost, "/v1/accounts/"+id+"/usageCosts:query", headers, body, &out); err != nil {
				return Result{}, err
			}
			for _, r := range out.Rows {
				if cur := r.Subtotal.CurrencyCode; cur != "" && !strings.EqualFold(cur, "USD") {
					continue // a budget in dollars cannot absorb another currency
				}
				hour, err := time.Parse(time.RFC3339, r.Dimensions.StartTime)
				if err != nil {
					continue
				}
				usd, err := r.Subtotal.USD()
				if err != nil {
					return Result{}, fmt.Errorf("fireworks: bad subtotal for %s: %w", r.Dimensions.Model, err)
				}
				res.Costs = append(res.Costs, Cost{Model: r.Dimensions.Model, Hour: hour.UTC(), USD: usd})
			}
			if strings.EqualFold(out.AttributionCompleteness, "INCOMPLETE") {
				res.Incomplete = true
			}
			if out.NextPageToken == "" {
				break
			}
			pageToken = out.NextPageToken
		}
	}
	return res, nil
}

// accountIDs lists the accounts the key can see.
func (f *Fireworks) accountIDs(ctx context.Context, headers map[string]string) ([]string, error) {
	key := headerFingerprint(headers)
	f.mu.Lock()
	if ids, ok := f.accounts[key]; ok {
		f.mu.Unlock()
		return ids, nil
	}
	f.mu.Unlock()
	var out struct {
		Accounts []struct {
			Name string `json:"name"`
		} `json:"accounts"`
	}
	if err := f.do(ctx, http.MethodGet, "/v1/accounts", headers, nil, &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, a := range out.Accounts {
		if id := strings.TrimPrefix(a.Name, "accounts/"); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("fireworks: the key sees no accounts")
	}
	f.mu.Lock()
	if f.accounts == nil {
		f.accounts = map[string][]string{}
	}
	f.accounts[key] = ids
	f.mu.Unlock()
	return ids, nil
}

// headerFingerprint keys the account cache by the credential. Hashed, so the
// secret is never a map key anything could print.
func headerFingerprint(h map[string]string) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(strings.ToLower(k))
		b.WriteByte(0)
		b.WriteString(h[k])
		b.WriteByte(1)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// do sends one API call. Errors name the status and the provider's message but
// never echo the request headers.
func (f *Fireworks) do(ctx context.Context, method, path string, headers map[string]string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(f.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.Client.Do(req)
	if err != nil {
		// net/http's message carries the request URL, which holds the account id.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("fireworks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("fireworks: HTTP %d: %s", resp.StatusCode, msg)
	}
	return json.Unmarshal(raw, out)
}

type usageCostsResponse struct {
	Rows []struct {
		Dimensions struct {
			Model     string `json:"model"`
			StartTime string `json:"startTime"`
		} `json:"dimensions"`
		Subtotal Money `json:"subtotal"`
	} `json:"rows"`
	AttributionCompleteness string `json:"attributionCompleteness"`
	NextPageToken           string `json:"nextPageToken"`
}

// Money is google.type.Money: whole units as a string (int64 over JSON) plus
// nanos. Both carry the same sign.
type Money struct {
	CurrencyCode string `json:"currencyCode"`
	Units        string `json:"units"`
	Nanos        int64  `json:"nanos"`
}

// USD is the amount as dollars.
func (m Money) USD() (float64, error) {
	var units int64
	if s := strings.TrimSpace(m.Units); s != "" {
		var err error
		if units, err = strconv.ParseInt(s, 10, 64); err != nil {
			return 0, err
		}
	}
	return float64(units) + float64(m.Nanos)/1e9, nil
}
