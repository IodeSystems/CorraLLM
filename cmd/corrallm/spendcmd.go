package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/api"
	"github.com/iodesystems/corrallm/internal/proxy"
)

// spendCall reaches the admin API with the admin token: this is the operator's
// command, not a client's. --key-file names another credential.
func spendCall(f *clientFlags, method, path string, in, out any) ([]byte, error) {
	key, err := f.key(false)
	if err != nil {
		return nil, err
	}
	if key == "" {
		b, err := os.ReadFile(filepath.Join(defaultHome(), "admin.token"))
		if err != nil {
			return nil, fmt.Errorf("no key: %w", err)
		}
		key = strings.TrimSpace(string(b))
	}
	return apiCall(method, f.base()+path, key, nil, in, out)
}

func spendText(rep proxy.SpendReport) string {
	if len(rep.Providers) == 0 {
		return "no Claude backend is paid for through corrallm\n"
	}
	var b strings.Builder
	for _, p := range rep.Providers {
		fmt.Fprintf(&b, "%s %s: estimated $%.2f (%s)\n", p.Provider, p.Period, p.EstimatedUSD, p.Source)
		if p.BilledUSD != nil {
			age := time.Duration(*p.StaleSeconds) * time.Second
			fmt.Fprintf(&b, "  last real figure: $%.2f, %s ago\n", *p.BilledUSD, age.Round(time.Minute))
		}
		if p.RemainingUSD != nil {
			fmt.Fprintf(&b, "  budget left: $%.2f\n", *p.RemainingUSD)
		} else {
			fmt.Fprintln(&b, "  no monthly budget set")
		}
	}
	return b.String()
}

func newSpendCmd() *cobra.Command {
	f := &clientFlags{}
	cmd := &cobra.Command{
		Use:   "spend",
		Short: "Show Claude spend this month; give the real figure; manage cost tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var rep proxy.SpendReport
			raw, err := spendCall(f, http.MethodGet, "/api/v1/spend", nil, &rep)
			if err != nil {
				return err
			}
			if f.json {
				printJSON(cmd.OutOrStdout(), raw)
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), spendText(rep))
			return nil
		},
	}
	f.bind(cmd)

	reconcile := &cobra.Command{
		Use:   "reconcile USD",
		Short: "Give the amount the Console shows this month; only spend after it is estimated",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			usd, err := strconv.ParseFloat(strings.TrimPrefix(args[0], "$"), 64)
			if err != nil || usd < 0 {
				return fmt.Errorf("reconcile: %q is not an amount in USD", args[0])
			}
			var rep proxy.SpendReport
			if _, err := spendCall(f, http.MethodPost, "/api/v1/spend/reconcile", map[string]any{"usd": usd}, &rep); err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), spendText(rep))
			return nil
		},
	}

	token := &cobra.Command{Use: "token", Short: "Cost tokens: they read GET /v1/spend and nothing else"}
	var label string
	var days int
	create := &cobra.Command{
		Use:   "create",
		Short: "Make a cost token and print it, the only time it is shown",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out struct {
				ID    int64  `json:"id"`
				Token string `json:"token"`
			}
			in := map[string]any{"label": label, "days": days}
			if _, err := spendCall(f, http.MethodPost, "/api/v1/spend/tokens", in, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", out.Token)
			fmt.Fprintf(cmd.ErrOrStderr(), "token %d; it is not shown again\n", out.ID)
			return nil
		},
	}
	create.Flags().StringVar(&label, "label", "", "what the token is for")
	create.Flags().IntVar(&days, "days", 0, "days until it expires (0 is 90, negative never)")
	list := &cobra.Command{
		Use:   "list",
		Short: "List cost tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out api.SpendTokensOutput
			raw, err := spendCall(f, http.MethodGet, "/api/v1/spend/tokens", nil, &out.Body)
			if err != nil {
				return err
			}
			if f.json {
				printJSON(cmd.OutOrStdout(), raw)
				return nil
			}
			for _, t := range out.Body.Tokens {
				state, day := "live", func(ms int64) string { return time.UnixMilli(ms).Format("2006-01-02") }
				switch {
				case t.RevokedMS != 0:
					state = "revoked " + day(t.RevokedMS)
				case t.ExpiresMS != 0 && t.ExpiresMS < time.Now().UnixMilli():
					state = "expired " + day(t.ExpiresMS)
				case t.ExpiresMS != 0:
					state = "expires " + day(t.ExpiresMS)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%s\n", t.ID, state, t.Label)
			}
			return nil
		},
	}
	revoke := &cobra.Command{
		Use:   "revoke ID",
		Short: "End a cost token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("revoke: %q is not a token id", args[0])
			}
			if _, err := spendCall(f, http.MethodPost, "/api/v1/spend/tokens/revoke", map[string]any{"id": id}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %d\n", id)
			return nil
		},
	}
	token.AddCommand(create, list, revoke)
	cmd.AddCommand(reconcile, token)
	return cmd
}
