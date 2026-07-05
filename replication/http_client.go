// Copyright Armada Contributors

package replication

import (
	"net/http"

	"github.com/armadakv/armada/security"
	"go.uber.org/zap"
)

// NewHTTPClient creates an HTTP client for fetching snapshots, reusing the replication TLS settings.
func NewHTTPClient(log *zap.SugaredLogger, leaderAddress string, cert, key, ca string, insecure bool, serverName string) (*http.Client, error) {
	var transport *http.Transport

	// Very simple check if we are using TLS
	if cert != "" || ca != "" || insecure {
		ti := security.TLSInfo{
			CertFile:           cert,
			KeyFile:            key,
			TrustedCAFile:      ca,
			InsecureSkipVerify: insecure,
			ServerName:         serverName,
			Logger:             log,
		}
		cfg, err := ti.ClientConfig()
		if err != nil {
			return nil, err
		}
		transport = &http.Transport{
			TLSClientConfig:   cfg,
			ForceAttemptHTTP2: true,
		}
	} else {
		transport = &http.Transport{
			ForceAttemptHTTP2: true,
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   0, // The context timeout will be used per download instead
	}, nil
}
