package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/iodesystems/corrallm/internal/agent"
	"github.com/iodesystems/corrallm/internal/config"
	"github.com/iodesystems/corrallm/internal/toolchain"
	"github.com/iodesystems/corrallm/internal/toolchain/recipes"
)

// `corrallm tools` — what runs the models, per host.
//
// Read-only by default and offline-capable: `list` asks each host what it has,
// which is a fork and an exec per tool, and only reaches the network for the
// drift check. That matters because this is the command someone runs when
// something is already wrong.

func newToolsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "Report the tools that run the models (llama.cpp, ninfer) per host",
	}
	cmd.AddCommand(newToolsListCmd(), newToolsPreflightCmd(), newToolsBuildCmd(), newToolsBuildsCmd(),
		newToolsActivateCmd(), newToolsInstallDepsCmd(), newToolsResolveCmd(), newToolsRecipesCmd())
	return cmd
}

// toolsRegistry builds a Registry over the configured hosts.
//
// The local/remote split is the SAME rule proc.Manager.hostFor uses — a server
// with an `agent:` block is reached through that agent, anything else is this
// machine. Duplicating the rule would be how the two quietly disagree about
// which box a command ran on.
func toolsRegistry(configPath string) (*toolchain.Registry, *config.Config, error) {
	cfg, _, err := liveConfig(configPath)
	if err != nil {
		return nil, nil, err
	}
	reg := &toolchain.Registry{
		Home: defaultHome(),
		Cfg:  func() *config.Config { return cfg },
		RunnerFor: func(server string) (toolchain.Runner, error) {
			srv, ok := cfg.Servers[server]
			if !ok {
				return nil, fmt.Errorf("no server %q in config", server)
			}
			if srv.Agent != nil {
				rh := agent.NewRemoteHost(server, srv.Agent.Endpoints, srv.Agent.ExpandedToken())
				return agent.NewToolRunner(rh), nil
			}
			// install-deps stays off on the primary's own machine too. Enabling
			// it is a per-host decision and the primary is a host.
			return &toolchain.Local{Server: server}, nil
		},
	}
	return reg, cfg, nil
}

func newToolsListCmd() *cobra.Command {
	var configPath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls", "status"},
		Short:   "What tool is installed on which host, at what version, and how far behind its pin",
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, cfg, err := toolsRegistry(configPath)
			if err != nil {
				return err
			}
			if len(cfg.Tools) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no tools declared (add a top-level `tools:` block)")
				return nil
			}
			states := reg.SurveyAll(context.Background())
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(states)
			}
			printToolStates(cmd.OutOrStdout(), states)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the corrallm YAML config")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable JSON output")
	return cmd
}

func printToolStates(w io.Writer, states []toolchain.State) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tHOST\tVERSION\tSOURCE\tDRIFT\tDETAIL")
	for _, s := range states {
		version, source, drift, detail := "-", "-", "-", ""
		switch {
		case s.Error != "":
			detail = s.Error
		case s.Probe == nil:
			detail = "not probed"
		case !s.Probe.Present:
			version = "absent"
			detail = s.Probe.Path
		default:
			version = s.Probe.Version
			source = s.Probe.Source
			if version == "" {
				// The honest rendering of ninfer-built-by-hand: present, and
				// there is no way to say what it is.
				version = "unknown"
				source = "unidentifiable"
			}
			detail = s.Probe.Path
		}
		if s.Drift != nil {
			// While PINNED, "behind" means behind the pin, not behind upstream:
			// a held tool is behind master on purpose, and the DRIFT column
			// showing that as a problem would nag about a decision. The target
			// is the pin, so it is the pin that gets named.
			target := s.Drift.RemoteHead
			if s.Drift.Pin != "" {
				target = s.Drift.Pin
			}
			switch {
			case s.Drift.Error != "":
				drift = "?"
			case s.Drift.Behind:
				drift = "BEHIND " + short(target)
			case s.Drift.Pin != "":
				drift = "pinned " + short(s.Drift.Pin)
				if s.Drift.Ahead {
					// What you are holding back FROM. The number a person who
					// pinned three weeks ago actually wants.
					drift += " (" + s.Drift.Ref + " at " + short(s.Drift.RemoteHead) + ")"
				}
			case s.Drift.RemoteHead != "":
				drift = "current"
			}
		}
		if s.Adopted {
			detail = "adopted: " + detail
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Tool, s.Host, version, source, drift, detail)
	}
	_ = tw.Flush()
}

func short(s string) string {
	if len(s) > 9 {
		return s[:9]
	}
	return s
}

func newToolsPreflightCmd() *cobra.Command {
	var configPath, server string
	cmd := &cobra.Command{
		Use:   "preflight <tool>",
		Short: "Can a host build this tool, and what is missing (seconds; compiles nothing)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, _, err := toolsRegistry(configPath)
			if err != nil {
				return err
			}
			host, err := pickHost(reg, args[0], server)
			if err != nil {
				return err
			}
			pf, err := reg.Preflight(context.Background(), args[0], host)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			status := "OK — this host can build it"
			if !pf.OK {
				status = "BLOCKED"
			}
			fmt.Fprintf(out, "%s on %s: %s\n", args[0], host, status)
			if !pf.Runnable {
				fmt.Fprintf(out, "  runnable here: NO\n")
			}
			for _, m := range pf.Missing {
				fmt.Fprintf(out, "  missing: %s\n", m)
			}
			for _, n := range pf.Notes {
				fmt.Fprintf(out, "  note:    %s\n", n)
			}
			for _, c := range pf.Commands {
				fmt.Fprintf(out, "  fix:     %s\n", c)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the corrallm YAML config")
	cmd.Flags().StringVar(&server, "server", "", "which host to ask (default: the only one declaring this tool)")
	return cmd
}

func newToolsBuildCmd() *cobra.Command {
	var configPath, server string
	var force, quiet bool
	cmd := &cobra.Command{
		Use:   "build <tool>",
		Short: "Pull and build a fresh copy on a host (ten to twenty minutes for a CUDA build)",
		Long: "Aligns the managed checkout to the tool's pinned ref, applies any patches, compiles\n" +
			"and installs it, then records a build stamp.\n\n" +
			"Refused on an ADOPTED install: a build starts with `git clean -xdf`, and an adopted\n" +
			"entry points at a tree corrallm does not own. Preflight runs first, so a missing\n" +
			"dependency costs a second rather than twelve minutes.\n\n" +
			"The stamp carries HEAD, the patch-set hash and the CUDA arch list, so a rebuild is\n" +
			"skipped only when all three still match — editing a patch or adding a GPU correctly\n" +
			"forces one. --force overrides.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, _, err := toolsRegistry(configPath)
			if err != nil {
				return err
			}
			host, err := pickHost(reg, args[0], server)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			// Progress goes to stderr as it happens. A quarter-hour of silence is
			// indistinguishable from a hang, and the operator is the one who has
			// to decide whether to wait.
			var progress io.Writer
			if !quiet {
				progress = cmd.ErrOrStderr()
			}

			fmt.Fprintf(out, "building %s on %s — this takes a while; ^C is safe (the compile keeps going)\n", args[0], host)
			res, err := reg.Build(cmd.Context(), args[0], host, force, progress)
			if err != nil {
				return err
			}
			if res.Skipped {
				fmt.Fprintf(out, "%s on %s: already current at %s (use --force to rebuild)\n", args[0], host, res.Stamp)
				return nil
			}
			fmt.Fprintf(out, "%s on %s: built %s in %ds\n", args[0], host, res.Version, res.Seconds)
			fmt.Fprintf(out, "  stamp: %s\n", res.Stamp)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the corrallm YAML config")
	cmd.Flags().StringVar(&server, "server", "", "which host to build on")
	cmd.Flags().BoolVar(&force, "force", false, "rebuild even when the stamp already matches")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress the live build log")
	return cmd
}

// `tools builds` and `tools activate` are the rollback pair.
//
// A build is expensive and occasionally wrong: llama.cpp ships several a day
// and any of them can regress a model that was fine an hour ago. Keeping the
// last few and being able to name one is what turns that from a twenty-minute
// recompile of a commit you have to go find into a rename.

func newToolsBuildsCmd() *cobra.Command {
	var configPath, server string
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "builds <tool>",
		Aliases: []string{"versions"},
		Short:   "List the builds installed on a host, newest first",
		Long: "Every build lands in its own directory and bin/ is a symlink naming the active\n" +
			"one, so the previous builds are still there to go back to. The last few are kept\n" +
			"(5 by default); the active build is never pruned, whatever its age.\n\n" +
			"Adopted installs report nothing: corrallm did not build them and keeps no history\n" +
			"of somebody else's tree.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, _, err := toolsRegistry(configPath)
			if err != nil {
				return err
			}
			host, err := pickHost(reg, args[0], server)
			if err != nil {
				return err
			}
			res, err := reg.Builds(cmd.Context(), args[0], host)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			if !res.Versioned {
				// Not an error: this is what every prefix looks like until the
				// first build after versioned installs landed.
				fmt.Fprintf(out, "%s on %s: not versioned yet — the current install becomes a build on the next `tools build`\n", args[0], host)
			}
			if len(res.Builds) == 0 {
				fmt.Fprintf(out, "no builds recorded on %s\n", host)
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "\tID\tBUILT\tHEAD")
			for _, b := range res.Builds {
				mark := " "
				if b.Active {
					mark = "*"
				}
				built := "-"
				if b.At > 0 {
					built = time.Unix(b.At, 0).Format("2006-01-02 15:04")
				}
				head := b.Head
				if len(head) > 12 {
					head = head[:12]
				}
				if head == "" {
					head = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mark, b.ID, built, head)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out, "* = active; keeping the newest %d\n", res.Keep)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the corrallm YAML config")
	cmd.Flags().StringVar(&server, "server", "", "which host to ask")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable JSON output")
	return cmd
}

func newToolsActivateCmd() *cobra.Command {
	var configPath, server string
	cmd := &cobra.Command{
		Use:     "activate <tool> <build-id>",
		Aliases: []string{"rollback", "use"},
		Short:   "Point a tool's bin/ at one of its installed builds (seconds; compiles nothing)",
		Long: "Makes an already-installed build current, by renaming a symlink.\n\n" +
			"Processes already running keep the binary they started with — they hold the inode,\n" +
			"not the path — so this takes effect on the NEXT spawn, exactly as a build does.\n" +
			"Unload the model to make it take effect now.\n\n" +
			"`tools builds <tool>` lists the ids.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, _, err := toolsRegistry(configPath)
			if err != nil {
				return err
			}
			host, err := pickHost(reg, args[0], server)
			if err != nil {
				return err
			}
			res, err := reg.Activate(cmd.Context(), args[0], host, args[1])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if res.Previous == res.Active {
				fmt.Fprintf(out, "%s on %s: already at %s\n", args[0], host, res.Active)
				return nil
			}
			fmt.Fprintf(out, "%s on %s: %s -> %s (takes effect on the next spawn)\n",
				args[0], host, orDash(res.Previous), res.Active)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the corrallm YAML config")
	cmd.Flags().StringVar(&server, "server", "", "which host to act on")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newToolsInstallDepsCmd() *cobra.Command {
	var configPath, server string
	cmd := &cobra.Command{
		Use:   "install-deps <tool>",
		Short: "Install the system packages preflight found missing (mutates the host; never scheduled)",
		Long: "Installs what `preflight` reported missing.\n\n" +
			"Refused unless that host's agent was started with --allow-install-deps, and refused\n" +
			"outright on an adopted install — corrallm does not manage dependencies for a build it\n" +
			"does not own. When refused, `preflight` still prints the exact command to run by hand.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, _, err := toolsRegistry(configPath)
			if err != nil {
				return err
			}
			host, err := pickHost(reg, args[0], server)
			if err != nil {
				return err
			}
			res, err := reg.InstallDeps(context.Background(), args[0], host)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, c := range res.Ran {
				fmt.Fprintf(out, "ran: %s\n", c)
			}
			if res.OK {
				fmt.Fprintf(out, "%s on %s: dependencies satisfied\n", args[0], host)
				return nil
			}
			return fmt.Errorf("%s", res.Error)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the corrallm YAML config")
	cmd.Flags().StringVar(&server, "server", "", "which host to act on")
	return cmd
}

// `tools resolve` answers what ${tool:x} becomes on a host, without spawning
// anything.
//
// Migrating a model's cmd to a tool reference is otherwise only checkable by
// loading the model, which on a remote host means pulling tens of GB of weights
// to find out whether a path was right. This is the same code path the spawn
// uses, so agreement here is agreement there.
func newToolsResolveCmd() *cobra.Command {
	var configPath, server string
	cmd := &cobra.Command{
		Use:   "resolve <tool>",
		Short: "Print what ${tool:<tool>} expands to on a host (spawns nothing)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, _, err := toolsRegistry(configPath)
			if err != nil {
				return err
			}
			host, err := pickHost(reg, args[0], server)
			if err != nil {
				return err
			}
			dir, err := reg.ToolDir(cmd.Context(), args[0], host)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "${tool:%s} on %s -> %s\n", args[0], host, dir)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the corrallm YAML config")
	cmd.Flags().StringVar(&server, "server", "", "which host to resolve for")
	return cmd
}

func newToolsRecipesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recipes",
		Short: "List the tool recipes built into this binary",
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, n := range recipes.Names() {
				fmt.Fprintln(cmd.OutOrStdout(), n)
			}
			return nil
		},
	}
}

// pickHost resolves which host a single-host command acts on.
//
// An explicit --server always wins. With one declared host the choice is
// unambiguous and asking would be ceremony; with several, refusing and naming
// them is better than picking one — these commands install packages and (later)
// start builds, and the wrong machine is a real cost.
func pickHost(reg *toolchain.Registry, tool, server string) (string, error) {
	cfg := reg.Cfg()
	t, ok := cfg.Tools[tool]
	if !ok {
		return "", fmt.Errorf("no tool %q declared (have: %s)", tool, strings.Join(toolNames(cfg), ", "))
	}
	if server != "" {
		if _, ok := t.Hosts[server]; !ok {
			return "", fmt.Errorf("tool %q is not declared on host %q", tool, server)
		}
		return server, nil
	}
	var hosts []string
	for h := range t.Hosts {
		hosts = append(hosts, h)
	}
	switch len(hosts) {
	case 0:
		return "", fmt.Errorf("tool %q is not declared on any host", tool)
	case 1:
		return hosts[0], nil
	default:
		return "", fmt.Errorf("tool %q is declared on %d hosts (%s) — name one with --server",
			tool, len(hosts), strings.Join(hosts, ", "))
	}
}

func toolNames(cfg *config.Config) []string {
	var out []string
	for n := range cfg.Tools {
		out = append(out, n)
	}
	return out
}
