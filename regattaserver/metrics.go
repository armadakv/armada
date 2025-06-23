// Copyright JAMF Software, LLC

package regattaserver

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/armadakv/armada/regattapb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// MetricsServer implements the Prometheus metrics gRPC service.
type MetricsServer struct {
	regattapb.UnimplementedMetricsServer
	registry prometheus.Gatherer
}

// NewMetricsServer creates a new instance of MetricsServer.
func NewMetricsServer(registry prometheus.Gatherer) *MetricsServer {
	// If no registry provided, use the default prometheus registry
	if registry == nil {
		registry = prometheus.DefaultGatherer
	}
	return &MetricsServer{
		registry: registry,
	}
}

// GetMetrics returns all metrics in Prometheus text format.
func (s *MetricsServer) GetMetrics(ctx context.Context, req *regattapb.MetricsRequest) (*regattapb.MetricsResponse, error) {
	if req.Format != "" {
		return nil, status.Error(codes.Unimplemented, "format is not yet supported")
	}

	var buf bytes.Buffer

	mfs, err := s.registry.Gather()
	if err != nil {
		return nil, fmt.Errorf("error gathering metrics: %w", err)
	}

	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	defer func() {
		if c, ok := enc.(expfmt.Closer); ok {
			_ = c.Close()
		}
	}()
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return nil, fmt.Errorf("error writing metric family to buffer: %w", err)
		}
	}

	metrics.WriteProcessMetrics(&buf)

	return &regattapb.MetricsResponse{
		MetricsData: buf.String(),
		Timestamp:   time.Now().Unix(),
	}, nil
}
