package config

// Config will hold the parsed contents of a gitloop config file (repository
// path, debounce timings, fetch interval, conflict-resolution policy, etc.).
//
// TODO: implement loading (e.g. from YAML) and validation.
type Config struct{}

// Load will read and parse the config file at path.
//
// TODO: implement.
func Load(path string) (*Config, error) {
	return nil, nil
}
