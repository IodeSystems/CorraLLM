package host

// Identity is what a running process can be recognised by later, by a process
// that did not start it.
//
// PIDS ARE REUSED, so a bare pid recorded yesterday names nothing reliable
// today. Start pins the instance (a reused pid has a different start time),
// and Cmdline pins what it is running (a backend that re-exec'd into something
// else, or a record whose command no longer matches, is not ours to adopt).
// Both are opaque strings, compared for equality only, and always produced by
// the same function on the same platform — so their format does not matter,
// only that it is stable for one process.
type Identity struct {
	Start   string `json:"start"`
	Cmdline string `json:"cmdline"`
}

// Matches reports whether two identities name the same process instance.
// Empty fields never match: "could not read it" must not compare equal to
// "could not read it".
func (a Identity) Matches(b Identity) bool {
	return a.Start != "" && a.Cmdline != "" && a.Start == b.Start && a.Cmdline == b.Cmdline
}
