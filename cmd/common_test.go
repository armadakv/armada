package cmd

import (
	"context"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestAuthFunc(t *testing.T) {
	// Test with empty token
	emptyTokenFunc := authFunc("")
	ctx := context.Background()
	newCtx, err := emptyTokenFunc(ctx)
	require.NoError(t, err)
	require.Equal(t, ctx, newCtx)

	// Test with non-empty token but no auth in context
	tokenFunc := authFunc("test-token")
	newCtx, err = tokenFunc(ctx)
	require.Error(t, err)

	// Note: Testing with auth in context would require mocking the auth.AuthFromMD function,
	// which is beyond the scope of this test.
}

func TestViperConfigReader(t *testing.T) {
	// Save original values
	originalMaintenanceToken := viper.GetString("maintenance.token")
	originalTablesToken := viper.GetString("tables.token")
	originalTestValue := viper.GetString("test.value")
	defer func() {
		viper.Set("maintenance.token", originalMaintenanceToken)
		viper.Set("tables.token", originalTablesToken)
		viper.Set("test.value", originalTestValue)
	}()

	// Set test values
	viper.Set("maintenance.token", "secret-maintenance-token")
	viper.Set("tables.token", "secret-tables-token")
	viper.Set("test.value", "test-value")

	// Get config
	config := viperConfigReader()

	// Check that sensitive values are masked
	require.Equal(t, "**********", config["maintenance.token"])
	require.Equal(t, "**********", config["tables.token"])
	require.Equal(t, "test-value", config["test.value"])
}

func TestTokenCredentials(t *testing.T) {
	// Test with empty token
	emptyToken := tokenCredentials("")
	metadata, err := emptyToken.GetRequestMetadata(context.Background())
	require.NoError(t, err)
	require.Empty(t, metadata)

	// Test with non-empty token
	token := tokenCredentials("test-token")
	metadata, err = token.GetRequestMetadata(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"authorization": "Bearer test-token"}, metadata)

	// Test RequireTransportSecurity
	require.True(t, token.RequireTransportSecurity())
}
