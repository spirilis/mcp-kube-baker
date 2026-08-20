// Package config loads mcp-kube-baker's YAML configuration. The server, auth,
// and logging sections are the generic-go-mcp library's own config types; this
// package adds the one field the library doesn't know about: the path to an
// external KUBECONFIG file.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	libconfig "github.com/spirilis/generic-go-mcp/config"
	"gopkg.in/yaml.v3"
)

// Config is the full mcp-kube-baker configuration.
type Config struct {
	// Kubeconfig is the path to a kubeconfig file external to this config
	// file. Required — cluster credentials are never inlined here.
	Kubeconfig string `yaml:"kubeconfig"`

	Server  libconfig.ServerConfig   `yaml:"server"`
	Auth    *libconfig.AuthConfig    `yaml:"auth,omitempty"`
	Logging *libconfig.LoggingConfig `yaml:"logging,omitempty"`
}

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	return LoadFromBytes(data)
}

// LoadFromBytes parses and validates configuration from YAML bytes.
func LoadFromBytes(data []byte) (*Config, error) {
	// Run the library's loader first so its defaulting and validation of the
	// shared sections (server mode, HTTP/unix defaults, logging) apply.
	libCfg, err := libconfig.LoadFromBytes(data)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	cfg.Server = libCfg.Server
	cfg.Auth = libCfg.Auth
	cfg.Logging = libCfg.Logging

	// The kubeconfig is deliberately NOT validated here: the caller resolves
	// the final path across all sources first (--kubeconfig flag > this file
	// > $KUBECONFIG > ~/.kube/config), then validates once. A file that omits
	// kubeconfig: is fine — the default resolution fills it in.
	return &cfg, nil
}

// ValidateKubeconfig checks that a kubeconfig path is set and readable. Call
// it exactly once, on the final resolved path, so every source is judged by
// the same rules.
func ValidateKubeconfig(path string) error {
	if path == "" {
		return fmt.Errorf("kubeconfig is required: set it in the config file, pass --kubeconfig, or set $KUBECONFIG")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("kubeconfig %q is not readable: %w", path, err)
	}
	return nil
}

// Default returns the zero-file configuration: stdio transport, legacy MCP
// compatibility enabled (a zero-config launch should serve any client, old or
// new), and logging to stderr at info/text. The kubeconfig is resolved via
// DefaultKubeconfigPath but not checked for existence — the caller validates
// the final path after applying any CLI override.
func Default() (*Config, error) {
	lib := libconfig.NewDefaultConfig()
	lib.Server.LegacyCompat = &libconfig.LegacyCompatConfig{Enabled: true}

	path, err := DefaultKubeconfigPath()
	if err != nil {
		return nil, err
	}
	return &Config{
		Kubeconfig: path,
		Server:     lib.Server,
		Auth:       lib.Auth,
		Logging:    lib.Logging,
	}, nil
}

// DefaultKubeconfigPath resolves the kubeconfig location kubectl-style:
// $KUBECONFIG if set (first entry if it is a list, since the kube layer loads
// a single file), else $HOME/.kube/config.
func DefaultKubeconfigPath() (string, error) {
	if env := os.Getenv("KUBECONFIG"); env != "" {
		if list := filepath.SplitList(env); len(list) > 0 && list[0] != "" {
			return list[0], nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no $KUBECONFIG set and home directory unknown: %w", err)
	}
	return filepath.Join(home, ".kube", "config"), nil
}
