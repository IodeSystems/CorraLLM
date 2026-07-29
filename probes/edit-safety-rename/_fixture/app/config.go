package app

// Config holds server settings.
type Config struct {
	Port int
	Name string
}

// Default returns the default Config.
func Default() Config {
	return Config{Port: 8080, Name: "app"}
}
