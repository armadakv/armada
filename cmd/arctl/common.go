// Copyright Armada Contributors

package main

import "context"

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
