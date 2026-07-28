package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/iodesystems/agentkit/llm"
	"github.com/spf13/cobra"
)

// `corrallm features` measures what a served model's tool surface actually does,
// as opposed to what its API advertises. Two endpoints that both speak
// OpenAI-compatible tool calling differ on every row of this report.
//
// It exists because every difference it finds is SILENT. A model that drops part
// of a tool argument, or ignores a grammar, returns 200 with a plausible-looking
// body; nothing in a normal agent loop reports it. The only way to know is to
// send a known string through a tool and compare what comes back.
//
// Read-only and cheap: one short generation per case against an already-served
// model. It spawns nothing and changes no state.
func newFeaturesCmd() *cobra.Command {
	var (
		baseURL, apiKey string
		asJSON          bool
		timeout         time.Duration
	)
	cmd := &cobra.Command{
		Use:   "features <model>",
		Short: "Probe a served model's tool-call and grammar support (read-only)",
		Long: "Probe a served model's tool-call surface: argument round-trip fidelity " +
			"and GBNF grammar support.\n\n" +
			"Grammar support is reported in two parts because they differ and the " +
			"difference decides an agent's design. llama.cpp accepts a grammar, but " +
			"REFUSES one alongside `tools` (\"Cannot specify grammar with tools\"), so " +
			"sampler-level enforcement and the native tool format are mutually " +
			"exclusive. A custom tool format parsed from content is the only shape " +
			"that can be grammar-constrained.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base := pick(baseURL, envOr("CORRALLM_BASE_URL", "http://127.0.0.1:8080/v1"))
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			c := llm.NewClient(base, pick(apiKey, envOr("CORRALLM_API_KEY", "")), args[0])
			rep, err := llm.ProbeToolSurface(ctx, c)
			if err != nil {
				return fmt.Errorf("probe %s at %s: %w", args[0], base, err)
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			printFeatures(cmd, rep, base)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&baseURL, "base-url", "", "OpenAI-compatible base URL (default http://127.0.0.1:8080/v1 or CORRALLM_BASE_URL)")
	f.StringVar(&apiKey, "api-key", "", "bearer token, if the endpoint needs one (or CORRALLM_API_KEY)")
	f.BoolVar(&asJSON, "json", false, "machine-readable JSON output")
	f.DurationVar(&timeout, "timeout", 5*time.Minute, "overall probe timeout")
	return cmd
}

func printFeatures(cmd *cobra.Command, rep llm.ToolSurfaceReport, base string) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "model    %s\nendpoint %s\n\n", rep.Model, base)

	fmt.Fprintf(w, "argument round-trip (a known string through a tool argument)\n")
	for _, e := range rep.Echo {
		status := "pass"
		detail := ""
		switch {
		case e.Err != "":
			status, detail = "ERROR", e.Err
		case e.NoCall:
			status, detail = "NO CALL", "the model returned no tool call"
		case e.BadJSON:
			status, detail = "BAD JSON", "arguments did not parse"
		case e.Lossy:
			// The interesting one, and the reason this command exists: a 200 with
			// an argument that is not what was sent, and no error anywhere.
			status = "LOSSY"
			detail = fmt.Sprintf("got %q", e.Got)
		}
		fmt.Fprintf(w, "  %-20s %-9s %s\n", e.Name, status, detail)
	}

	fmt.Fprintf(w, "\ngrammar (GBNF)\n")
	fmt.Fprintf(w, "  %-20s %v\n", "accepted", rep.GrammarAccepted)
	fmt.Fprintf(w, "  %-20s %v\n", "with tools", rep.GrammarWithToolsAccepted)
	if rep.GrammarError != "" {
		fmt.Fprintf(w, "  %-20s %s\n", "error", rep.GrammarError)
	}
	if rep.GrammarAccepted && !rep.GrammarWithToolsAccepted {
		fmt.Fprintf(w, "\n  Grammar and `tools` are mutually exclusive here. To constrain\n"+
			"  generation, use a tool-call format parsed from content\n"+
			"  (llm.HeredocGrammar / llm.ParseHeredocCalls) instead of `tools`.\n")
	}

	if !rep.OK() {
		fmt.Fprintf(w, "\n  Note: a LOSSY row means the model silently altered a tool argument.\n"+
			"  Measured cause on Qwen3: the model will not emit its own structural\n"+
			"  delimiter inside a value, so it truncates the remainder instead. That\n"+
			"  is model-side, not a parser bug, and no server config fixes it.\n")
	}
}
