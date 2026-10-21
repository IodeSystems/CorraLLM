//go:build linux

package sysmem

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// procRoot is /proc; a var so tests can point it at a fixture tree.
var procRoot = "/proc"

// GroupAnonBytes sums the ANONYMOUS resident memory (RssAnon) of every process
// in process group pgid.
//
// Anonymous, not the whole resident set: mmapped weights are file pages, and
// the kernel already counts those as available (MemAvailable) because it can
// drop them. Anonymous memory — weights read with --no-mmap, a KV cache in RAM
// — is what a group holds out of MemTotal − MemAvailable, so it is the part
// that can be subtracted from "used" without counting a byte twice.
func GroupAnonBytes(pgid int) (int64, error) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", procRoot, err)
	}
	var total int64
	found := false
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid directory
		}
		g, err := statPGID(fmt.Sprintf("%s/%d/stat", procRoot, pid))
		if err != nil || g != pgid {
			continue // gone mid-scan, or not ours
		}
		kb, err := rssAnonKB(fmt.Sprintf("%s/%d/status", procRoot, pid))
		if err != nil {
			continue // exited between the two reads; not fatal
		}
		total += kb * 1024
		found = true
	}
	if !found {
		return 0, fmt.Errorf("no processes in group %d", pgid)
	}
	return total, nil
}

// statPGID reads pgrp from a /proc/<pid>/stat file. comm is parenthesized and
// may contain spaces or parens, so fields are counted after the LAST ')'.
func statPGID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("%s: malformed", path)
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 3 {
		return 0, fmt.Errorf("%s: short", path)
	}
	return strconv.Atoi(f[2]) // state, ppid, pgrp
}

// rssAnonKB reads the RssAnon line of a /proc/<pid>/status file.
func rssAnonKB(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if key, kb, ok := parseMeminfoLine(sc.Text()); ok && key == "RssAnon" {
			return kb, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s: no RssAnon (a kernel thread?)", path)
}
