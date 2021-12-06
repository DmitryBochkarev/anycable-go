package wspc

type Config struct {
	// WS endpoint HTTP path
	Path string
	// WS endpoint HTTP port
	Port int
}

func NewConfig() Config {
	return Config{}
}

func (c *Config) Enabled() bool {
	return c.Path != ""
}
