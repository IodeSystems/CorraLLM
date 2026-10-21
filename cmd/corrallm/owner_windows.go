package main

import "os"

// selfIdentity on Windows carries no start stamp: there is no blue-green
// handoff there, so no other live writer shares the database.
func selfIdentity() (int, string) { return os.Getpid(), "" }

// processAlive is false for every other writer on Windows (see selfIdentity).
func processAlive(int64, string) bool { return false }
