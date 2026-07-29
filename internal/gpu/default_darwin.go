//go:build darwin

package gpu

// On macOS the vendor tool is not nvidia-smi and never will be. Selecting the
// prober at build time rather than probing for it keeps the default honest:
// NVIDIA{} here would fail on every call and report the machine as having no
// GPU, which is exactly wrong for a box whose GPU is the whole point.
func init() { Default = Apple{} }
