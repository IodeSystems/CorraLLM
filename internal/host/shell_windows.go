//go:build windows

package host

// shellFor renders a command string as an argv for this platform's shell.
//
// A caveat that matters more than the code: the cmd strings in config are
// written for POSIX sh. `cmd /C` does not understand `VAR=value prog`, `\` line
// continuations, or single quotes, so a spawn command authored on Linux will
// usually NOT run here unchanged. A Windows host needs its own cmd strings —
// which is fine, since it needs its own binary paths anyway.
func shellFor(cmd string) (string, []string) { return "cmd", []string{"/C", cmd} }
