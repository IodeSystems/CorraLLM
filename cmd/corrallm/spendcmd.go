package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/proxy"
	"github.com/iodesystems/corrallm/internal/store"
)

// spendTokenDays is how long a cost token lasts when nothing is asked for.
const spendTokenDays = 90

// spendDB opens the config and the database directly, as `config load` does: this
// is the operator's command, on the box, and needs no admin token, which may be
// disabled. What it writes reaches a running daemon by spendReload.
func spendDB(ctx context.Context) (*config.Config, *store.Store, func(), error) {
	p := derivePaths(defaultHome(), "", "")
	db, src, err := openConfigDB(p.db)
	if err != nil {
		return nil, nil, nil, err
	}
	defer db.Close()
	cfg, err := src.Load(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	st, err := store.Open(ctx, p.db)
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, st, func() { _ = st.Close() }, nil
}

// spendReload tells a running daemon to read what was written.
func spendReload(cmd *cobra.Command) {
	self, err := os.Executable()
	if err == nil {
		err = exec.Command(self, "service", "reload", "--name", "corrallm").Run()
	}
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "stored; the daemon has not read it: run `corrallm service reload`")
	}
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
			fmt.Fprintf(&b, "  budget left: $%.2f of a $%.2f limit, which resets %s\n", *p.RemainingUSD, *p.LimitUSD, p.ResetsAt)
			if p.TodayUSD != nil {
				fmt.Fprintf(&b, "  today (paced): $%.2f spent of $%.2f\n", *p.TodaySpentUSD, *p.TodayUSD)
			}
		} else {
			fmt.Fprintln(&b, "  no monthly budget set")
		}
		for _, t := range p.Budgets {
			state := fmt.Sprintf("$%.2f left", t.LeftUSD)
			if !t.Active {
				state = "ended"
			}
			fmt.Fprintf(&b, "  timed budget %d: $%.2f, $%.2f spent, %s, expires %s", t.ID, t.USD, t.SpentUSD, state, t.Expires)
			if t.Note != "" {
				fmt.Fprintf(&b, " (%s)", t.Note)
			}
			fmt.Fprintln(&b)
		}
	}
	return b.String()
}

func spendUSD(what, s string) (float64, error) {
	usd, err := strconv.ParseFloat(strings.TrimPrefix(s, "$"), 64)
	if err != nil || usd < 0 {
		return 0, fmt.Errorf("%s: %q is not an amount in USD", what, s)
	}
	return usd, nil
}

func printSpend(cmd *cobra.Command, cfg *config.Config, st *store.Store, asJSON bool) error {
	rep, err := proxy.ReportSpend(cfg, st, time.Now())
	if err != nil {
		return err
	}
	if asJSON {
		raw, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return nil
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), spendText(rep))
	return err
}

func newSpendCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "spend",
		Short: "Show Claude spend this period; correct it; manage timed budgets and cost tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, st, done, err := spendDB(cmd.Context())
			if err != nil {
				return err
			}
			defer done()
			return printSpend(cmd, cfg, st, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")

	var left bool
	reconcile := &cobra.Command{
		Use:   "reconcile USD",
		Short: "Correct the estimate, up or down, to what the Console shows spent (or left, with --left)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			usd, err := spendUSD("reconcile", args[0])
			if err != nil {
				return err
			}
			cfg, st, done, err := spendDB(cmd.Context())
			if err != nil {
				return err
			}
			defer done()
			if left {
				if usd, err = proxy.SpentFromLeft(cfg, usd); err != nil {
					return err
				}
			}
			if err := st.SetSpendReconcile("anthropic", usd, time.Now().UnixMilli()); err != nil {
				return err
			}
			spendReload(cmd)
			return printSpend(cmd, cfg, st, false)
		},
	}
	reconcile.Flags().BoolVar(&left, "left", false, "USD is what is left of the limit, not what was spent")

	cmd.AddCommand(reconcile, newSpendBudgetCmd(), newSpendTokenCmd())
	return cmd
}

func newSpendBudgetCmd() *cobra.Command {
	budget := &cobra.Command{Use: "budget", Short: "Timed budgets: dollars on a provider, past its hard limit, until an expiry"}
	var provider, ttl, expires, note string
	add := &cobra.Command{
		Use:   "add USD",
		Short: "Add a timed budget; it needs --ttl or --expires",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			usd, err := spendUSD("budget", args[0])
			if err != nil {
				return err
			}
			cfg, st, done, err := spendDB(cmd.Context())
			if err != nil {
				return err
			}
			defer done()
			if _, err := proxy.AddProviderBudget(cfg, st, provider, usd, ttl, expires, note, time.Now()); err != nil {
				return err
			}
			spendReload(cmd)
			return printSpend(cmd, cfg, st, false)
		},
	}
	add.Flags().StringVar(&provider, "provider", "anthropic", "the provider")
	expiry := func(c *cobra.Command) {
		c.Flags().StringVar(&ttl, "ttl", "", "time until it expires, e.g. 72h")
		c.Flags().StringVar(&expires, "expires", "", "when it expires, RFC 3339")
		c.Flags().StringVar(&note, "note", "", "what it is for")
	}
	expiry(add)

	var usdFlag, leftFlag string
	set := &cobra.Command{
		Use:   "set ID",
		Short: "Correct a timed budget: --usd, --left, --ttl, --expires, --note",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("set: %q is not a budget id", args[0])
			}
			e := proxy.BudgetEdit{TTL: ttl, Expires: expires}
			if usdFlag != "" {
				v, err := spendUSD("usd", usdFlag)
				if err != nil {
					return err
				}
				e.USD = &v
			}
			if leftFlag != "" {
				v, err := spendUSD("left", leftFlag)
				if err != nil {
					return err
				}
				e.Left = &v
			}
			if cmd.Flags().Changed("note") {
				e.Note = &note
			}
			cfg, st, done, err := spendDB(cmd.Context())
			if err != nil {
				return err
			}
			defer done()
			if err := proxy.EditProviderBudget(st, id, e, time.Now()); err != nil {
				return err
			}
			spendReload(cmd)
			return printSpend(cmd, cfg, st, false)
		},
	}
	expiry(set)
	set.Flags().StringVar(&usdFlag, "usd", "", "the amount")
	set.Flags().StringVar(&leftFlag, "left", "", "set the amount so that this much remains")

	rm := &cobra.Command{
		Use:   "rm ID",
		Short: "Remove a timed budget",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("rm: %q is not a budget id", args[0])
			}
			cfg, st, done, err := spendDB(cmd.Context())
			if err != nil {
				return err
			}
			defer done()
			if err := proxy.RemoveProviderBudget(st, id); err != nil {
				return err
			}
			spendReload(cmd)
			return printSpend(cmd, cfg, st, false)
		},
	}
	budget.AddCommand(add, set, rm)
	return budget
}

func newSpendTokenCmd() *cobra.Command {
	token := &cobra.Command{Use: "token", Short: "Cost tokens: they read GET /v1/spend and nothing else"}
	var label string
	var days int
	create := &cobra.Command{
		Use:   "create",
		Short: "Make a cost token and print it, the only time it is shown",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, st, done, err := spendDB(cmd.Context())
			if err != nil {
				return err
			}
			defer done()
			if days == 0 {
				days = spendTokenDays
			}
			tok, id, err := proxy.NewSpendToken(st, label, time.Duration(max(days, 0))*24*time.Hour, time.Now())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", tok)
			fmt.Fprintf(cmd.ErrOrStderr(), "token %d; it is not shown again\n", id)
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
			_, st, done, err := spendDB(cmd.Context())
			if err != nil {
				return err
			}
			defer done()
			rows, err := st.SpendTokens()
			if err != nil {
				return err
			}
			day := func(ms int64) string { return time.UnixMilli(ms).Format("2006-01-02") }
			for _, t := range rows {
				state := "live"
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
			_, st, done, err := spendDB(cmd.Context())
			if err != nil {
				return err
			}
			defer done()
			ok, err := st.RevokeSpendToken(id, time.Now().UnixMilli())
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no live cost token %d", id)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %d\n", id)
			return nil
		},
	}
	token.AddCommand(create, list, revoke)
	return token
}
