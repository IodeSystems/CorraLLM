package main

import (
	"errors"
	"os"
)

// upgradeSignals is empty on Windows: no listener can be passed on, so there
// is no handoff (restart instead).
func upgradeSignals() []os.Signal { return nil }

func sendUpgrade(int) error {
	return errors.New("handoff is not supported on Windows; restart instead")
}
