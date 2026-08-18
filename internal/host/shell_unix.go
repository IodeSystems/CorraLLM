//go:build unix

package host

// shellFor renders a command string as an argv for this platform's shell.
//
// `sh -c` is what corrallm has always used, and the cmd strings in config are
// written for it: pipes, environment prefixes (CUDA_VISIBLE_DEVICES=… before
// the binary), line continuations and quoting all assume POSIX sh.
func shellFor(cmd string) (string, []string) { return "sh", []string{"-c", cmd} }
