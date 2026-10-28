package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/config"
)

// Client side of corrallm's key API, for agents and people who hold a key:
// `corrallm request …` asks an admin for a key or for more on one
// (/v1/access-requests), `corrallm keys …` manages a key's subkeys (/v1/keys).
// Both talk to a running daemon over HTTP; nothing here reads the config or
// the database.
//
// The key is never taken as an argument: an argument lands in shell history
// and in every `ps` listing. It comes from CORRALLM_KEY or --key-file.

const defaultServerURL = "http://127.0.0.1:8111"

// clientFlags are the flags every client command shares.
type clientFlags struct {
	url     string
	keyFile string
	json    bool
}

func (f *clientFlags) bind(cmd *cobra.Command) {
	pf := cmd.PersistentFlags()
	pf.StringVar(&f.url, "url", "", "corrallm base URL (or CORRALLM_URL; default "+defaultServerURL+")")
	pf.StringVar(&f.keyFile, "key-file", "", "file holding your corrallm key (default: CORRALLM_KEY)")
	pf.BoolVar(&f.json, "json", false, "print the server's JSON instead of text")
}

func (f *clientFlags) base() string {
	u := f.url
	if u == "" {
		u = os.Getenv("CORRALLM_URL")
	}
	if u == "" {
		u = defaultServerURL
	}
	return strings.TrimRight(u, "/")
}

// key reads the caller's key; required says whether its absence is an error.
func (f *clientFlags) key(required bool) (string, error) {
	if f.keyFile != "" {
		b, err := os.ReadFile(f.keyFile)
		if err != nil {
			return "", err
		}
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
		return "", fmt.Errorf("%s is empty", f.keyFile)
	}
	if k := strings.TrimSpace(os.Getenv("CORRALLM_KEY")); k != "" {
		return k, nil
	}
	if required {
		return "", errors.New("no key: set CORRALLM_KEY or pass --key-file")
	}
	return "", nil
}

// apiCall sends one request and decodes a 2xx reply into out (when not nil).
// A non-2xx reply is an error carrying the server's message. raw is the reply
// body, for --json.
func apiCall(method, url, bearer string, headers map[string]string, in, out any) (raw []byte, err error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return raw, fmt.Errorf("%s %s: %d: %s", method, url, resp.StatusCode, msg)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, fmt.Errorf("%s %s: unreadable reply: %w", method, url, err)
		}
	}
	return raw, nil
}

// printJSON writes raw indented, for --json.
func printJSON(w io.Writer, raw []byte) {
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") != nil {
		_, _ = w.Write(raw)
		return
	}
	b.WriteByte('\n')
	_, _ = w.Write(b.Bytes())
}

// --- access and budget flags ---------------------------------------------

const budgetSyntax = "SELECTOR=LIMIT, e.g. '*=$20/calendar-day', 'model:*deepseek*=$3/calendar-day,hard', " +
	"'local=1000req/hour'. SELECTOR: * | local | proxy | provider:<name> | cred:<provider>/<name> | model:<name>. " +
	"LIMIT: " + config.LimitSyntax

// parseBudgets reads repeated --budget flags. Several for one selector add up
// to that selector's limit list.
func parseBudgets(ss []string) (config.KeyBudgets, error) {
	out := config.KeyBudgets{}
	for _, s := range ss {
		sel, lim, ok := strings.Cut(s, "=")
		if !ok || strings.TrimSpace(sel) == "" {
			return nil, fmt.Errorf("budget %q: want %s", s, budgetSyntax)
		}
		l, err := config.ParseLimit(lim)
		if err != nil {
			return nil, fmt.Errorf("budget %q: %w", s, err)
		}
		sel = config.NormalizeSelector(sel)
		if err := config.ValidateSelector(sel); err != nil {
			return nil, fmt.Errorf("budget %q: %w", s, err)
		}
		out[sel] = append(out[sel], l)
	}
	return out, nil
}

// parseModels reads --models (comma-separated, repeatable) into selectors.
func parseModels(ss []string) ([]string, error) {
	var out []string
	for _, s := range ss {
		for _, part := range strings.Split(s, ",") {
			if strings.TrimSpace(part) != "" {
				out = append(out, part)
			}
		}
	}
	out = config.NormalizeSelectors(out)
	for _, sel := range out {
		if err := config.ValidateSelector(sel); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// budgetsText renders budgets one selector at a time: "model:x $3/calendar-day,hard".
func budgetsText(b config.KeyBudgets) string {
	if len(b) == 0 {
		return "no budgets"
	}
	parts := make([]string, 0, len(b))
	for _, sel := range b.Selectors() {
		ls := make([]string, 0, len(b[sel]))
		for _, l := range b[sel] {
			ls = append(ls, l.Compact())
		}
		parts = append(parts, sel+" "+strings.Join(ls, " "))
	}
	return strings.Join(parts, "; ")
}

func modelsText(m []string) string {
	if len(m) == 0 {
		return "every model"
	}
	return strings.Join(m, ", ")
}
