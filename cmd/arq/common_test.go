// Copyright Armada Contributors

package main

import (
	"context"
	"testing"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/require"
)

func TestInitConfigEnvironment(t *testing.T) {
	oldConfig := k
	k = koanf.New(".")
	t.Cleanup(func() { k = oldConfig })
	t.Setenv("ARMADA_WRITE_OUT", "json")
	t.Setenv("ARMADA_COMMAND_TIMEOUT", "5s")

	require.NoError(t, initConfig(app))
	require.Equal(t, "json", k.String("write-out"))
	require.Equal(t, 5*time.Second, k.Duration("command-timeout"))
}

func TestConfiguredCommandTimeout(t *testing.T) {
	original := k.Get("command-timeout")
	t.Cleanup(func() { k.Set("command-timeout", original) })

	k.Set("command-timeout", "invalid")
	_, err := configuredCommandTimeout()
	require.ErrorContains(t, err, "invalid --command-timeout")

	k.Set("command-timeout", "250ms")
	timeout, err := configuredCommandTimeout()
	require.NoError(t, err)
	require.Equal(t, 250*time.Millisecond, timeout)
}

func TestCommandContext(t *testing.T) {
	original := k.Duration("command-timeout")
	t.Cleanup(func() { k.Set("command-timeout", original) })
	k.Set("command-timeout", time.Second)

	ctx, cancel := commandContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(time.Second), deadline, 100*time.Millisecond)
}

func TestDial(t *testing.T) {
	original := map[string]any{
		"address": k.String("address"),
		"token":   k.String("token"),
		"ca":      k.String("ca"),
		"cert":    k.String("cert"),
		"key":     k.String("key"),
	}
	t.Cleanup(func() {
		for key, value := range original {
			k.Set(key, value)
		}
	})

	set := func(address, token string) {
		k.Set("address", address)
		k.Set("token", token)
		k.Set("ca", "")
		k.Set("cert", "")
		k.Set("key", "")
	}

	t.Run("requires address scheme", func(t *testing.T) {
		set("127.0.0.1:8443", "")
		_, err := dial()
		require.ErrorContains(t, err, "must include a valid scheme")
	})

	t.Run("allows plaintext without token", func(t *testing.T) {
		set("http://127.0.0.1:8443", "")
		conn, err := dial()
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})

	t.Run("refuses token over plaintext", func(t *testing.T) {
		set("http://127.0.0.1:8443", "secret")
		_, err := dial()
		require.ErrorContains(t, err, "refusing to send --token")
	})

	t.Run("allows token with TLS", func(t *testing.T) {
		set("https://127.0.0.1:8443", "secret")
		conn, err := dial()
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})
}
