package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/config"
)

// `corrallm request …` files an access request (POST /v1/access-requests) and
// follows it. The claim secret the server returns is the only proof of who
// filed, and the issued key is handed over ONCE, so both are saved to
// <home>/requests/<id>.json (0600) before anything else happens: a closed
// terminal must not lose a key.

// savedRequest is one filed request, as kept on disk.
type savedRequest struct {
	ID      string          `json:"id"`
	Claim   string          `json:"claim"`
	URL     string          `json:"url"`
	Kind    string          `json:"kind"` // new | more
	Label   string          `json:"label,omitempty"`
	Filed   string          `json:"filed"`
	Status  string          `json:"status"`
	Reason  string          `json:"reason,omitempty"`
	Key     string          `json:"key,omitempty"`
	Granted json.RawMessage `json:"granted,omitempty"`
}

func requestsDir(home string) string { return filepath.Join(home, "requests") }

func saveRequest(home string, r savedRequest) error {
	dir := requestsDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	tmp := filepath.Join(dir, r.ID+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, r.ID+".json"))
}

func loadRequest(home, id string) (savedRequest, error) {
	var r savedRequest
	b, err := os.ReadFile(filepath.Join(requestsDir(home), id+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, fmt.Errorf("no saved request %s in %s (the claim is kept only by the machine that filed it)", id, requestsDir(home))
		}
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

func loadRequests(home string) ([]savedRequest, error) {
	ents, err := os.ReadDir(requestsDir(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []savedRequest
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		r, err := loadRequest(home, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filed < out[j].Filed })
	return out, nil
}

// askFlags are the flags `request new` and `request more` share.
type askFlags struct {
	label, purpose, from, group, ttl, expires, token string
	models, budgets, valueAdds                       []string
	mint, wait                                       bool
	interval                                         time.Duration
}

func (a *askFlags) bind(cmd *cobra.Command, withLabel bool) {
	f := cmd.Flags()
	if withLabel {
		f.StringVar(&a.label, "label", "", "name for the new key (required)")
	}
	f.StringVar(&a.purpose, "purpose", "", "what the access is for (required)")
	f.StringVar(&a.from, "from", "", "who is asking, e.g. the agent's name")
	f.StringSliceVar(&a.models, "models", nil, "selectors the key may see and use, comma-separated: local, proxy, provider:<name>, cred:<provider>/<name>, model:<name> (default: every model)")
	f.StringArrayVar(&a.budgets, "budget", nil, "a budget, repeatable: "+budgetSyntax)
	f.StringArrayVar(&a.valueAdds, "value-add", nil, "a one-time amount spent after the budgets refuse, repeatable: $USD[@MODEL][,TTL], e.g. '$10@deepseek,2d'")
	f.StringVar(&a.group, "group", "", "priority group for the key")
	f.StringVar(&a.ttl, "ttl", "", "key expires this long after approval: 48h, 7d")
	f.StringVar(&a.expires, "expires", "", "key expiry, RFC 3339")
	f.BoolVar(&a.mint, "mint", false, "ask for permission to mint subkeys")
	f.BoolVar(&a.wait, "wait", false, "wait for the decision; a new key is then printed on stdout")
	f.DurationVar(&a.interval, "interval", 10*time.Second, "how often --wait polls")
	f.StringVar(&a.token, "request-token", "", "shared filing token, if the server's rules ask for one (or CORRALLM_REQUEST_TOKEN)")
}

// body builds the AccessAsk JSON with only the flags that were set: for a
// request about an existing key, an absent field leaves that part as it is.
func (a *askFlags) body(cmd *cobra.Command) (map[string]any, error) {
	if strings.TrimSpace(a.purpose) == "" {
		return nil, errors.New("--purpose: say what the access is for")
	}
	b := map[string]any{"purpose": a.purpose}
	set := func(name, key string, v any) {
		if cmd.Flags().Changed(name) {
			b[key] = v
		}
	}
	set("label", "label", a.label)
	set("from", "from", a.from)
	set("group", "group", a.group)
	set("ttl", "ttl", a.ttl)
	set("expires", "expires", a.expires)
	set("mint", "mint", a.mint)
	if cmd.Flags().Changed("models") {
		m, err := parseModels(a.models)
		if err != nil {
			return nil, err
		}
		b["models"] = m
	}
	if cmd.Flags().Changed("budget") {
		bs, err := parseBudgets(a.budgets)
		if err != nil {
			return nil, err
		}
		b["budgets"] = bs
	}
	var vas []map[string]any
	for _, s := range a.valueAdds {
		v, err := parseValueAdd(s)
		if err != nil {
			return nil, err
		}
		vas = append(vas, v)
	}
	if len(vas) > 0 {
		b["valueAdds"] = vas
	}
	return b, nil
}

// parseValueAdd reads "$USD[@MODEL][,TTL]".
func parseValueAdd(s string) (map[string]any, error) {
	rest := strings.TrimSpace(s)
	v := map[string]any{}
	if i := strings.Index(rest, ","); i >= 0 {
		v["ttl"], rest = strings.TrimSpace(rest[i+1:]), rest[:i]
	}
	if i := strings.Index(rest, "@"); i >= 0 {
		v["model"], rest = strings.TrimSpace(rest[i+1:]), rest[:i]
	}
	n, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(rest), "$"), 64)
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("value add %q: want $USD[@MODEL][,TTL], e.g. '$10@deepseek,2d'", s)
	}
	v["usd"] = n
	return v, nil
}

func newRequestCmd() *cobra.Command {
	var cf clientFlags
	var home string
	cmd := &cobra.Command{
		Use:   "request",
		Short: "Ask the owner for a key, or for more on yours (access requests)",
		Long: "Files an access request with a running corrallm and follows it. The owner approves or denies it\n" +
			"on the dashboard's Approvals page. The claim secret and any issued key are saved under\n" +
			"<home>/requests/ (0600); a new key is handed over by the server only once.\n\n" +
			"A subkey asks for more with its own key (CORRALLM_KEY); its parent can also change it directly\n" +
			"with `corrallm keys set`.",
	}
	cf.bind(cmd)
	cmd.PersistentFlags().StringVar(&home, "home", defaultHome(), "corrallm home; requests are kept in <home>/requests")

	file := func(kind string, a *askFlags) func(*cobra.Command, []string) error {
		return func(c *cobra.Command, _ []string) error {
			if kind == "new" && strings.TrimSpace(a.label) == "" {
				return errors.New("--label: name the key you are asking for")
			}
			body, err := a.body(c)
			if err != nil {
				return err
			}
			key := ""
			if kind == "more" {
				if key, err = cf.key(true); err != nil {
					return err
				}
			}
			token := a.token
			if token == "" {
				token = os.Getenv("CORRALLM_REQUEST_TOKEN")
			}
			var out struct {
				ID     string `json:"id"`
				Claim  string `json:"claim"`
				Status string `json:"status"`
			}
			base := cf.base()
			raw, err := apiCall(http.MethodPost, base+"/v1/access-requests", key,
				map[string]string{"X-Corrallm-Request-Token": token}, body, &out)
			if err != nil {
				return err
			}
			r := savedRequest{ID: out.ID, Claim: out.Claim, URL: base, Kind: kind, Label: a.label,
				Filed: time.Now().UTC().Format(time.RFC3339), Status: out.Status}
			if err := saveRequest(home, r); err != nil {
				return fmt.Errorf("filed %s, but could not save its claim: %w", out.ID, err)
			}
			if cf.json {
				printJSON(c.OutOrStdout(), raw)
			} else if !a.wait {
				fmt.Fprintln(c.OutOrStdout(), out.ID)
			}
			fmt.Fprintf(c.ErrOrStderr(), "filed %s (pending); saved to %s\n", out.ID, filepath.Join(requestsDir(home), out.ID+".json"))
			if !a.wait {
				fmt.Fprintf(c.ErrOrStderr(), "check with: corrallm request status %s [--wait]\n", out.ID)
				return nil
			}
			return waitFor(c, home, r, a.interval, cf.json)
		}
	}

	var na, ma askFlags
	newCmd := &cobra.Command{
		Use:   "new",
		Short: "Ask for a new key",
		Long: "Asks for a new key. No key is sent. With --wait, the issued key is printed alone on stdout:\n\n" +
			"  KEY=$(corrallm request new --label bench --purpose \"benchmark runs\" \\\n" +
			"        --models local,provider:fireworks --budget 'provider:fireworks=$5/calendar-day,hard' --wait)",
		Args: cobra.NoArgs,
		RunE: file("new", &na),
	}
	na.bind(newCmd, true)
	moreCmd := &cobra.Command{
		Use:   "more",
		Short: "Ask for more on your own key (CORRALLM_KEY or --key-file)",
		Long: "Asks the owner to change your key: budgets, models, value adds, expiry, mint permission.\n" +
			"Only the flags you set are asked for; the rest of the key stays as it is.\n\n" +
			"  corrallm request more --purpose \"deepseek runs out by noon\" --value-add '$10@deepseek,2d'",
		Args: cobra.NoArgs,
		RunE: file("more", &ma),
	}
	ma.bind(moreCmd, false)

	var wait bool
	var interval time.Duration
	statusCmd := &cobra.Command{
		Use:     "status [ID...]",
		Aliases: []string{"list"},
		Short:   "Show saved requests and their decisions (all of them when no ID is given)",
		RunE: func(c *cobra.Command, args []string) error {
			var rs []savedRequest
			if len(args) == 0 {
				all, err := loadRequests(home)
				if err != nil {
					return err
				}
				rs = all
			}
			for _, id := range args {
				r, err := loadRequest(home, id)
				if err != nil {
					return err
				}
				rs = append(rs, r)
			}
			if len(rs) == 0 {
				fmt.Fprintf(c.ErrOrStderr(), "no saved requests in %s\n", requestsDir(home))
				return nil
			}
			for _, r := range rs {
				if wait && len(args) > 0 {
					if err := waitFor(c, home, r, interval, cf.json); err != nil {
						return err
					}
					continue
				}
				if r.Status == "pending" {
					var err error
					if r, err = pollRequest(home, r); err != nil {
						fmt.Fprintf(c.ErrOrStderr(), "%s: %v\n", r.ID, err)
					}
				}
				printRequest(c.OutOrStdout(), r)
			}
			return nil
		},
	}
	statusCmd.Flags().BoolVar(&wait, "wait", false, "wait for the named requests to be decided")
	statusCmd.Flags().DurationVar(&interval, "interval", 10*time.Second, "how often --wait polls")

	cmd.AddCommand(newCmd, moreCmd, statusCmd)
	return cmd
}

// pollRequest asks the server for a decision and saves what it says.
func pollRequest(home string, r savedRequest) (savedRequest, error) {
	var out struct {
		Status  string          `json:"status"`
		Reason  string          `json:"reason"`
		Granted json.RawMessage `json:"granted"`
		Key     string          `json:"key"`
	}
	if _, err := apiCall(http.MethodGet, r.URL+"/v1/access-requests/"+r.ID, r.Claim, nil, nil, &out); err != nil {
		return r, err
	}
	r.Status, r.Reason = out.Status, out.Reason
	if len(out.Granted) > 0 {
		r.Granted = out.Granted
	}
	if out.Key != "" {
		r.Key = out.Key
	}
	return r, saveRequest(home, r)
}

// waitFor polls until r is decided. An approved new key goes alone to stdout.
func waitFor(c *cobra.Command, home string, r savedRequest, every time.Duration, asJSON bool) error {
	if every < time.Second {
		every = time.Second
	}
	said := false
	for r.Status == "pending" {
		if !said {
			fmt.Fprintf(c.ErrOrStderr(), "waiting for a decision on %s (Ctrl-C is safe: resume with corrallm request status %s --wait)\n", r.ID, r.ID)
			said = true
		}
		time.Sleep(every)
		var err error
		if r, err = pollRequest(home, r); err != nil {
			fmt.Fprintf(c.ErrOrStderr(), "%s: %v (retrying)\n", r.ID, err)
		}
	}
	if asJSON {
		b, _ := json.Marshal(r)
		printJSON(c.OutOrStdout(), b)
	}
	switch {
	case r.Status == "denied":
		return fmt.Errorf("%s was denied: %s", r.ID, firstOf(r.Reason, "no reason given"))
	case r.Kind == "new" && r.Key == "":
		return fmt.Errorf("%s was approved, but the key was already collected; it is not saved in %s", r.ID, requestsDir(home))
	case r.Kind == "new":
		if !asJSON {
			fmt.Fprintln(c.OutOrStdout(), r.Key)
		}
		fmt.Fprintf(c.ErrOrStderr(), "%s approved; the key is also saved in %s\n", r.ID, filepath.Join(requestsDir(home), r.ID+".json"))
	default:
		if !asJSON {
			fmt.Fprintln(c.OutOrStdout(), "approved")
		}
		fmt.Fprintf(c.ErrOrStderr(), "%s approved%s\n", r.ID, grantedWords(r.Granted))
	}
	return nil
}

func printRequest(w io.Writer, r savedRequest) {
	what := "more on your key"
	if r.Kind == "new" {
		what = "new key " + strconv.Quote(r.Label)
	}
	line := fmt.Sprintf("%s  %-8s  %s  filed %s", r.ID, r.Status, what, r.Filed)
	if r.Reason != "" {
		line += "  reason: " + r.Reason
	}
	if r.Key != "" {
		line += "  key: " + r.Key
	}
	fmt.Fprintln(w, line+grantedWords(r.Granted))
}

// grantedWords summarizes what an approval granted.
func grantedWords(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var g struct {
		Models    *[]string          `json:"models"`
		Budgets   *config.KeyBudgets `json:"budgets"`
		ValueAdds []struct {
			USD   float64 `json:"usd"`
			Model string  `json:"model"`
		} `json:"valueAdds"`
		TTL     string `json:"ttl"`
		Expires string `json:"expires"`
	}
	if json.Unmarshal(raw, &g) != nil {
		return ""
	}
	var parts []string
	if g.Models != nil {
		parts = append(parts, "models: "+modelsText(*g.Models))
	}
	if g.Budgets != nil {
		parts = append(parts, "budgets: "+budgetsText(*g.Budgets))
	}
	for _, v := range g.ValueAdds {
		s := fmt.Sprintf("value add $%g", v.USD)
		if v.Model != "" {
			s += " on " + v.Model
		}
		parts = append(parts, s)
	}
	if g.TTL != "" || g.Expires != "" {
		parts = append(parts, "expires "+firstOf(g.Expires, "in "+g.TTL))
	}
	if len(parts) == 0 {
		return ""
	}
	return "  granted: " + strings.Join(parts, "; ")
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
