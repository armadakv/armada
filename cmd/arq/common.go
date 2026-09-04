// Copyright Armada Contributors

package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/armadakv/armada/security"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/v2"
	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

var k = koanf.New(".")

// initConfig loads flag defaults, ARMADA_ environment variables, and explicit
// command-line flags, in increasing order of precedence.
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

	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		return err
	}
	if err := k.Load(env.Provider("ARMADA_", ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "ARMADA_")), "_", "-")
	}), nil); err != nil {
		return err
	}
	return k.Load(confmap.Provider(explicit, "."), nil)
}

// dial creates a gRPC client connection to the configured Armada address.
func dial() (*grpc.ClientConn, error) {
	rawAddr := k.String("address")
	u, err := url.Parse(rawAddr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "unix" && u.Scheme != "unixs") {
		return nil, fmt.Errorf("address %q must include a valid scheme (one of http, https, unix, unixs)", rawAddr)
	}

	addr := u.Host
	network := "tcp"
	if u.Scheme == "unix" || u.Scheme == "unixs" {
		addr = u.Host + u.Path
		network = "unix"
	}
	secure := u.Scheme == "https" || u.Scheme == "unixs"
	token := k.String("token")

	var transport grpc.DialOption
	if secure {
		ti := security.TLSInfo{
			CertFile:           k.String("cert"),
			KeyFile:            k.String("key"),
			TrustedCAFile:      k.String("ca"),
			ServerName:         k.String("server-name"),
			InsecureSkipVerify: k.Bool("insecure-skip-verify"),
		}
		cfg, err := ti.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("cannot build tls config: %w", err)
		}
		transport = grpc.WithTransportCredentials(credentials.NewTLS(cfg))
	} else {
		if token != "" {
			return nil, fmt.Errorf("refusing to send --token over an insecure %q connection; use an https or unixs address", u.Scheme)
		}
		transport = grpc.WithTransportCredentials(insecure.NewCredentials())
	}

	if network == "unix" {
		addr = "unix://" + addr
	} else {
		addr = "dns:" + addr
	}

	opts := []grpc.DialOption{transport}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(tokenCredentials(token)))
	}
	return grpc.NewClient(addr, opts...)
}

type tokenCredentials string

func (t tokenCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	if t == "" {
		return nil, nil
	}
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}

func (tokenCredentials) RequireTransportSecurity() bool {
	return true
}
