package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/iodesystems/corrallm/internal/proxy"
)

// The account view (P74) as text: `corrallm spend` per provider, `corrallm keys
// list` per budget and subkey.

func usd(n float64) string {
	if n < 0 {
		return fmt.Sprintf("-$%.2f", -n)
	}
	return fmt.Sprintf("$%.2f", n)
}

// writeAccount prints an account's available amount, chunks and ceilings.
func writeAccount(w io.Writer, indent string, a *proxy.Account) {
	if a == nil {
		return
	}
	if a.AvailableUSD == nil {
		fmt.Fprintf(w, "%savailable: no hard dollar limit\n", indent)
	} else {
		fmt.Fprintf(w, "%savailable: %s\n", indent, usd(*a.AvailableUSD))
	}
	if a.Overdrawn && a.OverdrawnUSD != nil {
		fmt.Fprintf(w, "%sOVERDRAWN by %s: refused until a refill or a top-up\n", indent, usd(*a.OverdrawnUSD))
	}
	for _, c := range a.Chunks {
		kind := c.Kind
		if c.Banked {
			kind += ", banked"
		}
		line := fmt.Sprintf("%schunk %s (%s): %s left of %s", indent, c.Name, kind, usd(c.LeftUSD), usd(c.USD))
		if c.RefillsAt != "" {
			line += ", refills " + c.RefillsAt
		}
		if c.Expires != "" {
			line += ", expires " + c.Expires
		}
		if c.Note != "" && (c.Kind == "budget" || c.Kind == "value-add") {
			line += " (" + c.Note + ")"
		}
		fmt.Fprintln(w, line)
	}
	for _, c := range a.Ceilings {
		line := fmt.Sprintf("%sceiling %s %s: %s used of %s", indent, c.Scope, c.Window, usd(c.UsedUSD), usd(c.USD))
		if c.ResetsAt != "" {
			line += ", resets " + c.ResetsAt
		}
		fmt.Fprintln(w, line)
	}
	if len(a.DrainOrder) > 1 {
		fmt.Fprintf(w, "%sdrain: %s (order: %s)\n", indent, a.Drain, strings.Join(a.DrainOrder, ", "))
	}
	if a.DrainNote != "" {
		fmt.Fprintf(w, "%sdrain note: %s\n", indent, a.DrainNote)
	}
}

// writeKeyBudgets prints each selector's effective amount and its own account.
func writeKeyBudgets(w io.Writer, indent string, bs []proxy.KeyBudgetAccount) {
	if len(bs) == 0 {
		fmt.Fprintf(w, "%sno hard dollar budget\n", indent)
	}
	for _, b := range bs {
		line := indent + b.Selector + ": "
		if b.EffectiveUSD != nil {
			line += fmt.Sprintf("effective %s (limited by %s)", usd(*b.EffectiveUSD), b.LimitedBy)
		} else {
			line += "effective: no limit"
		}
		if b.Account == nil {
			line += "; no budget of its own"
		}
		fmt.Fprintln(w, line)
		writeAccount(w, indent+"  ", b.Account)
	}
}

// printKeyAccount prints the `account` object of a GET /v1/keys reply. A reply
// from a server without it prints nothing.
func printKeyAccount(w io.Writer, raw []byte) {
	var out struct {
		Self struct {
			Account *proxy.KeyAccount `json:"account"`
		} `json:"self"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Self.Account == nil {
		return
	}
	a := out.Self.Account
	fmt.Fprintln(w, "  account:")
	writeKeyBudgets(w, "    ", a.Budgets)
	for _, s := range a.Subkeys {
		label := s.Label
		if label == "" {
			label = "(no label)"
		}
		fmt.Fprintf(w, "  subkey %s: %s spent today (UTC)\n", label, usd(s.SpentTodayUSD))
		writeKeyBudgets(w, "    ", s.Budgets)
	}
}
