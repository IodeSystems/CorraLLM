package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/service"
)

// newServiceLoginAdminCmd opens the installed instance's dashboard signed in
// with its admin token.
//
// It answers the question a fresh install leaves open — where is it and what
// do I type — from the two things that know: the unit (its ADDR) and the
// running daemon (/health names its token file). The token travels in the URL
// fragment, which a browser never sends to the server; the dashboard stores it
// and strips it from the address bar.
func newServiceLoginAdminCmd() *cobra.Command {
	var (
		name    string
		base    string
		printIt bool
	)
	cmd := &cobra.Command{
		Use:   "login-admin",
		Short: "Open the dashboard signed in with the admin token",
		Long: "Open the dashboard signed in with the admin token.\n\n" +
			"Finds the address from the installed unit's ADDR (default :6502), asks the\n" +
			"running daemon where its admin token is, and opens\n" +
			"http://127.0.0.1:<port>/#token=<token> in the browser. --print prints that URL\n" +
			"instead (for a browser on another machine, pass --url with an address it can reach).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if base == "" {
				base = localBase(installedEnv(name)["ADDR"])
			}
			base = strings.TrimRight(base, "/")
			hc := &http.Client{Timeout: 5 * time.Second}

			var health struct {
				TokenPath string `json:"tokenPath"`
				Insecure  bool   `json:"insecure"`
			}
			if err := getJSON(hc, base+"/health", &health); err != nil {
				return fmt.Errorf("nothing answers at %s (%v) — is it running? `corrallm service status --name %s`", base, err, name)
			}
			target := base + "/"
			if health.Insecure {
				fmt.Fprintf(out, "%s runs with --insecure: no token needed\n", base)
			} else {
				var who struct {
					AdminToken string `json:"adminToken"`
				}
				if err := getJSON(hc, base+"/auth/whoami", &who); err == nil && who.AdminToken == "disabled" {
					return fmt.Errorf("the admin token is disabled on %s: `corrallm auth admin-token enable`, then `corrallm service reload --name %s` "+
						"(or sign in with SSO at %s/)", base, name, base)
				}
				b, err := os.ReadFile(health.TokenPath)
				if err != nil {
					return fmt.Errorf("read the admin token the daemon names (%s): %w", health.TokenPath, err)
				}
				target += "#token=" + url.QueryEscape(strings.TrimSpace(string(b)))
				fmt.Fprintf(out, "admin token: %s\n", health.TokenPath)
			}
			if printIt {
				fmt.Fprintln(out, target)
				return nil
			}
			if err := openBrowser(target); err != nil {
				// No browser here (an ssh session): the URL is the answer.
				fmt.Fprintf(out, "could not open a browser (%v); open this:\n%s\n", err, target)
				return nil
			}
			fmt.Fprintf(out, "opened %s/ in the browser\n", base)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", service.DefaultName, "unit name (without .service), to find its ADDR")
	f.StringVar(&base, "url", "", "dashboard base URL (default: http://127.0.0.1:<port from the unit's ADDR>)")
	f.BoolVar(&printIt, "print", false, "print the sign-in URL instead of opening it (it contains the token)")
	return cmd
}

// installedEnv is the environment the installed unit or LaunchAgent sets.
func installedEnv(name string) map[string]string {
	if useLaunchd() {
		p, err := launchdPlistPath(name)
		if err != nil {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		return service.PlistEnv(string(b))
	}
	dir, err := service.UserUnitDir()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, name+".service"))
	if err != nil {
		return nil
	}
	return service.UnitEnv(string(b))
}

// localBase turns a listen address into a URL this machine's browser can
// open: all-interfaces and empty hosts become 127.0.0.1. serve's default
// listen address is :6502.
func localBase(addr string) string {
	if addr == "" {
		addr = ":6502"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func getJSON(hc *http.Client, u string, v any) error {
	resp, err := hc.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func openBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Run()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Run()
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return fmt.Errorf("no display")
		}
		return exec.Command("xdg-open", u).Run()
	}
}
