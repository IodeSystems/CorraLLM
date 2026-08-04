package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"
)

// A build id is the identity of a BINARY, not of a release.
//
// The version string cannot do this job. An untagged build stamps "dev", so an
// agent running yesterday's dev build and a primary serving today's both say
// "dev", and self-update concludes there is nothing to do — precisely during
// development, when getting a change onto an attached machine matters most.
// Version strings distinguish releases, and most builds are not releases.
//
// Hashing the file distinguishes every build from every other, needs no
// discipline from whoever ran the build, and — because the agent installs the
// served file verbatim — lets both sides compute the SAME identity for the same
// bytes. Matching identities are what terminate the update loop; without that,
// an agent whose binary differs from the primary's advertised version by
// construction would re-exec on every heartbeat, forever.
//
// It lives in this package rather than agentdist because agentdist already
// imports this one. Two implementations that drifted by a byte would leave
// every agent updating in a loop, so there is exactly one.
const buildIDLen = 12

// HashFile returns the short content hash identifying a binary.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil))[:buildIDLen], nil
}

var (
	ownOnce sync.Once
	ownID   string
)

// OwnBuildID is this process's own build id, computed once.
//
// Once, not per heartbeat: it cannot change while the process runs — an update
// re-execs rather than mutating this image — and hashing ~37 MB on every beat
// would be pure waste. Empty when the executable cannot be read (deleted out
// from under us, or an OS that will not say where it is), which callers treat
// as "identity unavailable" and fall back to comparing version strings.
func OwnBuildID() string {
	ownOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		if id, err := HashFile(exe); err == nil {
			ownID = id
		}
	})
	return ownID
}
