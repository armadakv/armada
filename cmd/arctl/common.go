// Copyright Armada Contributors

package main

import (
	"context"
	"strings"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/v2"
	"github.com/urfave/cli/v3"
)

var k = koanf.New(".")

// initConfig loads configuration into the koanf instance with flat, dot-separated
// keys. Sources are loaded from lowest to highest priority: flag defaults <
// environment variables < explicitly set command-line flags.
func initConfig(c *cli.Command) error {
	defaults := make(map[string]any)
	explicit := make(map[string]any)
	for _, f := range c.Flags {
		name := f.Names()[0]
		if c.IsSet(name) {
			explicit[name] = c.Value(name)
		} else {
			defaults[name] = c.Value(name)
		}
	}

	// 1. Flag defaults (lowest priority).
	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		return err
	}

	// 2. Environment variables prefixed with ARMADA_. The prefix is stripped and
	// remaining underscores are converted to dots so ARMADA_ADDRESS maps to the
	// "address" key.
	if err := k.Load(env.Provider("ARMADA_", ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "ARMADA_")), "_", ".")
	}), nil); err != nil {
		return err
	}

	// 3. Explicitly set command-line flags (highest priority).
	if err := k.Load(confmap.Provider(explicit, "."), nil); err != nil {
		return err
	}

	return nil
}

type tokenCredentials string

func (t tokenCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	if t != "" {
		return map[string]string{"authorization": "Bearer " + string(t)}, nil
	}
	return nil, nil
}

func (tokenCredentials) RequireTransportSecurity() bool {
	return true
}
