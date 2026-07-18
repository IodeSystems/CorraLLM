package greet

// Greet returns a friendly greeting for name.
func Greet(name string) string {
	// BUG: the expected greeting is "Hello, <name>!".
	return "Hi, " + name
}
