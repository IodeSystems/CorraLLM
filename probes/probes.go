// Package probes embeds the built-in probe library into the binary.
//
// Why embed at all: `--tasks-dir` defaulted to the relative string "probes",
// resolved from the RUNNER'S working directory. That is only correct when
// llm-bench is invoked from the repo root — and the daemon that spawns it is
// not: the running corrallm's cwd is the ml-kit deployment directory, so the
// default resolved to a path that does not exist and the flag had to be passed
// as an absolute `/home/<user>/...`. A machine-specific absolute path in a
// service flag is the same defect as one in a go.mod replace, relocated.
//
// Embedded, the library travels with the binary: `llm-bench run` works from any
// directory, on any machine, with no flag. `--tasks-dir` keeps working and
// becomes what it should always have been — an OVERRIDE for user-defined
// probes, not a requirement for the built-ins.
//
// `all:` matters. Probe fixtures deliberately contain dotfiles (a .gitignore
// inside a fixture workspace is part of what a coding probe is asked to
// respect), and the default embed pattern silently skips names beginning with
// "." or "_". Without `all:` those fixtures would embed incomplete and the
// probe would measure something subtly different from what it measures on disk.
package probes

import "embed"

// FS is the built-in probe library: one directory per probe, each holding
// task.yaml or probe.md plus its fixture tree.
//
// Not usable directly by the loader — a Task's fixtures must exist as real
// files, because the workspace is seeded by copying them and the MCP tool
// server jails the agent inside a real directory. See task.MaterializeBuiltins.
//
//go:embed all:*
var FS embed.FS
