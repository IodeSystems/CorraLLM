//go:build !unix

package handoff

import (
	"net"
	"time"
)

// Predecessor is unavailable without descriptor passing.
type Predecessor struct{}

// Start always fails here: there is no way to pass a listening socket on.
func Start(string, []string, net.Listener, time.Duration) (*Predecessor, error) {
	return nil, ErrUnsupported
}

func (p *Predecessor) PID() int           { return 0 }
func (p *Predecessor) Send(Status) error  { return ErrUnsupported }
func (p *Predecessor) Done() <-chan error { return nil }

// Successor is never inherited here.
type Successor struct {
	Listener       net.Listener
	PredecessorPID int
}

func Inherit() (*Successor, error)              { return nil, nil }
func (s *Successor) Ready() error               { return ErrUnsupported }
func (s *Successor) Watch(func(Status), func()) {}
func ReportStartFailure(error)                  {}
func Notify(string) (bool, error)               { return false, nil }
func CheckSupervisor() error                    { return ErrUnsupported }
