package keel

import (
	"github.com/go-keel/keel/core/kconfig"
)

type Config kconfig.Config

// NewConfig creates a new Config instance from the given origin config.
func NewConfig(origin *kconfig.Config) *Config {
	return (*Config)(origin)
}
