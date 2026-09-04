// Copyright Armada Contributors

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDial(t *testing.T) {
	// Save and restore the global config keys touched by dial().
	origAddress := k.String("address")
	origToken := k.String("token")
	origCA := k.String("ca")
	origCert := k.String("cert")
	origKey := k.String("key")
	t.Cleanup(func() {
		k.Set("address", origAddress)
		k.Set("token", origToken)
		k.Set("ca", origCA)
		k.Set("cert", origCert)
		k.Set("key", origKey)
	})

	set := func(address, token, ca string) {
		k.Set("address", address)
		k.Set("token", token)
		k.Set("ca", ca)
		k.Set("cert", "")
		k.Set("key", "")
	}

	t.Run("missing scheme is rejected", func(t *testing.T) {
		set("127.0.0.1:8443", "", "")
		_, err := dial()
		require.Error(t, err)
		require.Contains(t, err.Error(), "must include a valid scheme")
	})

	t.Run("plaintext http without token succeeds", func(t *testing.T) {
		set("http://127.0.0.1:8443", "", "")
		conn, err := dial()
		require.NoError(t, err)
		require.NotNil(t, conn)
		require.NoError(t, conn.Close())
	})

	t.Run("token is refused over an insecure connection", func(t *testing.T) {
		set("http://127.0.0.1:8443", "secret", "")
		_, err := dial()
		require.Error(t, err)
		require.Contains(t, err.Error(), "insecure")
	})

	t.Run("https with token succeeds", func(t *testing.T) {
		set("https://127.0.0.1:8443", "secret", "")
		conn, err := dial()
		require.NoError(t, err)
		require.NotNil(t, conn)
		require.NoError(t, conn.Close())
	})

	t.Run("missing CA file is reported", func(t *testing.T) {
		set("https://127.0.0.1:8443", "", filepath.Join(t.TempDir(), "missing.crt"))
		_, err := dial()
		require.Error(t, err)
	})

	t.Run("client cert without key is rejected", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "client.crt")
		require.NoError(t, os.WriteFile(p, []byte("cert"), 0o600))
		set("https://127.0.0.1:8443", "", "")
		k.Set("cert", p)
		_, err := dial()
		require.Error(t, err)
	})

	t.Run("unixs address with token succeeds", func(t *testing.T) {
		set("unixs:///tmp/armada.sock", "secret", "")
		conn, err := dial()
		require.NoError(t, err)
		require.NotNil(t, conn)
		require.NoError(t, conn.Close())
	})
}
