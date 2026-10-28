package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/config"
)

// `corrallm keys …` manages the subkeys of the caller's own key (/v1/keys):
// mint one per project, change its models, budgets and expiry, switch it off,
// revoke it. The key must be allowed to mint (`mint: true`); ask for that with
// `corrallm request more --mint`.

// keyView is a key as /v1/keys shows it.
type keyView struct {
	Key      string                  `json:"key"`
	Label    string                  `json:"label"`
	Models   []string                `json:"models"`
	Budgets  config.KeyBudgets       `json:"budgets"`
	Expires  string                  `json:"expires"`
	Disabled bool                    `json:"disabled"`
	Windows  map[string][]windowView `json:"windows"`
	Warnings []string                `json:"warnings"`
}

type windowView struct {
	Label   string  `json:"label"`
	Limit   float64 `json:"limit"`
	Used    float64 `json:"used"`
	Blocked bool    `json:"blocked"`
}

// accessFlags are the models/budget/expiry flags `keys mint` and `keys set`
// share.
type accessFlags struct {
	label, ttl, expires  string
	models, budgets      []string
	allModels, noBudgets bool
	noExpiry             bool
}

func (a *accessFlags) bind(cmd *cobra.Command, forSet bool) {
	f := cmd.Flags()
	f.StringVar(&a.label, "label", "", "the key's name")
	f.StringSliceVar(&a.models, "models", nil, "selectors it may see and use, comma-separated: local, proxy, provider:<name>, cred:<provider>/<name>, model:<name> (default: every model the parent may use)")
	f.StringArrayVar(&a.budgets, "budget", nil, "a budget, repeatable: "+budgetSyntax)
	f.StringVar(&a.ttl, "ttl", "", "expires this long from now: 48h, 7d")
	f.StringVar(&a.expires, "expires", "", "expiry, RFC 3339")
	if forSet {
		f.BoolVar(&a.allModels, "all-models", false, "remove the models filter (every model the parent may use)")
		f.BoolVar(&a.noBudgets, "no-budgets", false, "remove every budget")
		f.BoolVar(&a.noExpiry, "no-expiry", false, "remove the expiry")
	}
}

// body has only what was set. --budget replaces ALL budgets, --models the
// whole filter: the server stores each as one value.
func (a *accessFlags) body(cmd *cobra.Command) (map[string]any, error) {
	b := map[string]any{}
	ch := cmd.Flags().Changed
	if ch("label") {
		b["label"] = a.label
	}
	switch {
	case ch("models") && ch("all-models"):
		return nil, fmt.Errorf("--models and --all-models together")
	case ch("models"):
		m, err := parseModels(a.models)
		if err != nil {
			return nil, err
		}
		b["models"] = m
	case a.allModels:
		b["models"] = []string{}
	}
	switch {
	case ch("budget") && a.noBudgets:
		return nil, fmt.Errorf("--budget and --no-budgets together")
	case ch("budget"):
		bs, err := parseBudgets(a.budgets)
		if err != nil {
			return nil, err
		}
		b["budgets"] = bs
	case a.noBudgets:
		b["budgets"] = config.KeyBudgets{}
	}
	n := 0
	for _, f := range []string{"ttl", "expires", "no-expiry"} {
		if ch(f) {
			n++
		}
	}
	if n > 1 {
		return nil, fmt.Errorf("--ttl, --expires and --no-expiry: use one")
	}
	switch {
	case ch("ttl"):
		b["ttl"] = a.ttl
	case ch("expires"):
		b["expires"] = a.expires
	case a.noExpiry:
		b["ttl"] = ""
	}
	return b, nil
}

func newKeysCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage your key's subkeys: mint, change budgets and models, expire, revoke",
		Long: "Manages the subkeys of your own key (CORRALLM_KEY or --key-file) on a running corrallm.\n" +
			"A subkey may use only what both it and your key permit, and its requests count against\n" +
			"both its budgets and yours. Subkey budgets may add up to more than yours (your budget\n" +
			"stops the total); the server warns when they do.\n\n" +
			"Your own key's budgets are the owner's: ask with `corrallm request more`.",
	}
	cf.bind(cmd)

	call := func(c *cobra.Command, method, path string, body any) ([]byte, error) {
		key, err := cf.key(true)
		if err != nil {
			return nil, err
		}
		return apiCall(method, cf.base()+path, key, nil, body, nil)
	}

	listCmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"info"},
		Short:   "Your key's models, budgets and spend, and every subkey (any key may run it)",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			raw, err := call(c, http.MethodGet, "/v1/keys", nil)
			if err != nil {
				return err
			}
			if cf.json {
				printJSON(c.OutOrStdout(), raw)
				return nil
			}
			var out struct {
				Self struct {
					Models    []string                `json:"models"`
					Budgets   config.KeyBudgets       `json:"budgets"`
					Windows   map[string][]windowView `json:"windows"`
					Warnings  []string                `json:"warnings"`
					Expires   string                  `json:"expires"`
					ValueAdds []struct {
						USD, Spent float64
						Model      string
						Active     bool
					} `json:"valueAdds"`
				} `json:"self"`
				Keys []keyView `json:"keys"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return err
			}
			w := c.OutOrStdout()
			fmt.Fprintln(w, "your key")
			fmt.Fprintf(w, "  models:  %s\n", modelsText(out.Self.Models))
			printBudgets(w, out.Self.Budgets, out.Self.Windows)
			for _, v := range out.Self.ValueAdds {
				if v.Active {
					fmt.Fprintf(w, "  value add: $%.2f of $%.2f left%s\n", v.USD-v.Spent, v.USD, onModel(v.Model))
				}
			}
			if out.Self.Expires != "" {
				fmt.Fprintf(w, "  expires: %s\n", out.Self.Expires)
			}
			for _, s := range out.Self.Warnings {
				fmt.Fprintf(w, "  warning: %s\n", s)
			}
			if len(out.Keys) == 0 {
				fmt.Fprintln(w, "\nno subkeys (mint one with: corrallm keys mint --label NAME)")
				return nil
			}
			sort.SliceStable(out.Keys, func(i, j int) bool { return out.Keys[i].Label < out.Keys[j].Label })
			for _, k := range out.Keys {
				state := "on"
				if k.Disabled {
					state = "off"
				}
				fmt.Fprintf(w, "\n%s  %s  (%s)\n", k.Key, firstOf(k.Label, "(no label)"), state)
				fmt.Fprintf(w, "  models:  %s\n", modelsText(k.Models))
				printBudgets(w, k.Budgets, k.Windows)
				if k.Expires != "" {
					fmt.Fprintf(w, "  expires: %s\n", k.Expires)
				}
			}
			return nil
		},
	}

	var ma accessFlags
	mintCmd := &cobra.Command{
		Use:   "mint",
		Short: "Mint a subkey; its key is printed alone on stdout",
		Long: "Mints a subkey of your key. It is live at once.\n\n" +
			"  corrallm keys mint --label proj-a --models local,model:*deepseek* \\\n" +
			"      --budget 'model:*deepseek*=$1.70/calendar-day,hard' --ttl 30d",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			body, err := ma.body(c)
			if err != nil {
				return err
			}
			raw, err := call(c, http.MethodPost, "/v1/keys", body)
			if err != nil {
				return err
			}
			return printKey(c, raw, cf.json, true)
		},
	}
	ma.bind(mintCmd, false)

	var sa accessFlags
	setCmd := &cobra.Command{
		Use:   "set KEY",
		Short: "Change a subkey: label, models, budgets, expiry",
		Long: "Changes only what you pass. --budget replaces ALL of the subkey's budgets and --models its\n" +
			"whole filter, so list every one you want to keep.\n\n" +
			"  corrallm keys set ck-… --budget '*=$4/calendar-day' --budget 'model:*deepseek*=$2/calendar-day,hard'",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			body, err := sa.body(c)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to change: pass --label, --models, --budget, --ttl or another change")
			}
			raw, err := call(c, http.MethodPatch, "/v1/keys/"+url.PathEscape(args[0]), body)
			if err != nil {
				return err
			}
			return printKey(c, raw, cf.json, false)
		},
	}
	sa.bind(setCmd, true)

	toggle := func(use, short string, off bool) *cobra.Command {
		return &cobra.Command{
			Use:   use + " KEY",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				raw, err := call(c, http.MethodPatch, "/v1/keys/"+url.PathEscape(args[0]), map[string]any{"disabled": off})
				if err != nil {
					return err
				}
				return printKey(c, raw, cf.json, false)
			},
		}
	}
	revokeCmd := &cobra.Command{
		Use:   "revoke KEY",
		Short: "Delete a subkey; it answers 401 from then on",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			raw, err := call(c, http.MethodDelete, "/v1/keys/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			if cf.json {
				printJSON(c.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "revoked %s\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(listCmd, mintCmd, setCmd,
		toggle("disable", "Switch a subkey off, keeping it and its budgets", true),
		toggle("enable", "Switch a subkey back on", false),
		revokeCmd)
	return cmd
}

// printKey shows one key from a /v1/keys reply. A minted key goes alone to
// stdout so `KEY=$(corrallm keys mint …)` works; the rest goes to stderr.
func printKey(c *cobra.Command, raw []byte, asJSON, minted bool) error {
	if asJSON {
		printJSON(c.OutOrStdout(), raw)
		return nil
	}
	var k keyView
	if err := json.Unmarshal(raw, &k); err != nil {
		return err
	}
	w := c.ErrOrStderr()
	if minted {
		fmt.Fprintln(c.OutOrStdout(), k.Key)
	} else {
		w = c.OutOrStdout()
		fmt.Fprintln(w, k.Key)
	}
	state := "on"
	if k.Disabled {
		state = "off"
	}
	fmt.Fprintf(w, "  label:   %s (%s)\n", firstOf(k.Label, "(none)"), state)
	fmt.Fprintf(w, "  models:  %s\n", modelsText(k.Models))
	printBudgets(w, k.Budgets, k.Windows)
	if k.Expires != "" {
		fmt.Fprintf(w, "  expires: %s\n", k.Expires)
	}
	for _, s := range k.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", s)
	}
	return nil
}

// printBudgets shows each budget with its window's spend.
func printBudgets(w io.Writer, b config.KeyBudgets, windows map[string][]windowView) {
	if len(b) == 0 {
		fmt.Fprintln(w, "  budgets: none")
		return
	}
	fmt.Fprintln(w, "  budgets:")
	for _, sel := range b.Selectors() {
		for _, l := range b[sel] {
			line := fmt.Sprintf("    %-28s %s", sel, l.Compact())
			for _, win := range windows[sel] {
				if win.Label == l.Label() {
					line += "  " + spentWords(l.Dimension(), win)
				}
			}
			fmt.Fprintln(w, line)
		}
	}
}

func spentWords(dim string, w windowView) string {
	amt := func(n float64) string {
		if dim == config.DimUSD {
			return fmt.Sprintf("$%.2f", n)
		}
		return fmt.Sprintf("%.0f", n)
	}
	s := amt(w.Used) + " of " + amt(w.Limit) + " used"
	if w.Blocked {
		s += " — FULL"
	}
	return s
}

func onModel(m string) string {
	if m == "" {
		return ""
	}
	return " on " + strings.TrimSpace(m)
}
