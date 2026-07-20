package app

import "fmt"

// Server serves requests.
type Server struct {
	cfg Config
}

// NewServer builds a Server from a Config.
func NewServer(c Config) *Server {
	return &Server{cfg: c}
}

func (s *Server) Addr() string {
	return fmt.Sprintf(":%d", s.cfg.Port)
}
