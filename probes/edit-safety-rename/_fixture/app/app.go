package app

// Boot wires the app from the default configuration.
func Boot() string {
	c := Default()
	s := NewServer(c)
	return s.Addr() + " " + Describe(c)
}
