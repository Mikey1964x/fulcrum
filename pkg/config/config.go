// Package config loads the command's settings from a .env file and the
// process environment.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/joho/godotenv"
)

const (
	envShareURL  = "FULCRUM_SHARE_URL"
	envUsername  = "FULCRUM_USERNAME"
	envPassword  = "FULCRUM_PASSWORD"
	envOutputDir = "FULCRUM_OUTPUT_DIR"

	defaultOutputDir = "data"
)

// Config is the validated configuration of a run.
type Config struct {
	ShareURL  string
	Username  string
	Password  string
	OutputDir string
}

// Load reads envPath (if it exists) and builds the configuration. Variables
// already set in the process environment win over the file.
func Load(envPath string) (*Config, error) {
	if err := godotenv.Load(envPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("reading %s: %w", envPath, err)
	}

	cfg := &Config{
		ShareURL:  strings.TrimSpace(os.Getenv(envShareURL)),
		Username:  strings.TrimSpace(os.Getenv(envUsername)),
		Password:  os.Getenv(envPassword),
		OutputDir: strings.TrimSpace(os.Getenv(envOutputDir)),
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = defaultOutputDir
	}

	if cfg.ShareURL == "" {
		return nil, fmt.Errorf("%s is not set; add it to %s", envShareURL, envPath)
	}
	u, err := url.Parse(cfg.ShareURL)
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid URL: %w", envShareURL, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%s must be an https URL", envShareURL)
	}
	if !strings.HasSuffix(strings.ToLower(u.Path), ".geojson") {
		return nil, fmt.Errorf("%s must point at the .geojson format of the share", envShareURL)
	}
	return cfg, nil
}

// ShareName returns the share token from the URL, used as the default base
// name of the output files.
func (c *Config) ShareName() string {
	u, err := url.Parse(c.ShareURL)
	if err != nil {
		return "share"
	}
	base := path.Base(u.Path)
	name := strings.TrimSuffix(base, path.Ext(base))
	if name == "" || name == "." || name == "/" {
		return "share"
	}
	return name
}
