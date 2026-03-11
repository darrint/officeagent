package config

// Config holds the application configuration.
type Config struct {
	// Addr is the TCP address the HTTP server listens on.
	Addr string
}

// Default returns a Config populated with default values.
func Default() *Config {
	return &Config{
		Addr: "127.0.0.1:8080",
	}
}
