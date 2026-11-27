// Copyright Armada Contributors

package watch

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics are the Watch API metrics of one node.
type metrics struct {
	watchers   *prometheus.GaugeVec
	events     prometheus.Counter
	catchUps   prometheus.Counter
	catchUpDur prometheus.Histogram
	overflows  prometheus.Counter
	canceled   *prometheus.CounterVec
}

func newMetrics() *metrics {
	return &metrics{
		watchers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "armada_watch_watchers",
			Help: "Number of registered watchers, by state: synced watchers are fed live, unsynced ones read history from the store.",
		}, []string{"state"}),
		events: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "armada_watch_events_sent_total",
			Help: "Number of events queued for watch streams.",
		}),
		catchUps: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "armada_watch_catch_ups_total",
			Help: "Number of times a watcher started reading its events from the store, after a gap, a slow stream or a historical start revision.",
		}),
		catchUpDur: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "armada_watch_catch_up_duration_seconds",
			Help:    "Time a watcher spent reading its events from the store before it was fed live again.",
			Buckets: prometheus.ExponentialBuckets(0.001, 4, 10),
		}),
		overflows: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "armada_watch_queue_overflows_total",
			Help: "Number of committed batches a hub could not queue, whose events watchers had to read from the store.",
		}),
		canceled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "armada_watch_canceled_total",
			Help: "Number of canceled watchers, by reason.",
		}, []string{"reason"}),
	}
}

func (m *metrics) Describe(ch chan<- *prometheus.Desc) {
	m.watchers.Describe(ch)
	m.events.Describe(ch)
	m.catchUps.Describe(ch)
	m.catchUpDur.Describe(ch)
	m.overflows.Describe(ch)
	m.canceled.Describe(ch)
}

func (m *metrics) Collect(ch chan<- prometheus.Metric) {
	m.watchers.Collect(ch)
	m.events.Collect(ch)
	m.catchUps.Collect(ch)
	m.catchUpDur.Collect(ch)
	m.overflows.Collect(ch)
	m.canceled.Collect(ch)
}

func (m *metrics) added() {
	m.watchers.WithLabelValues(stateLabel(stateSynced)).Inc()
}

func (m *metrics) moved(from, to watcherState) {
	m.watchers.WithLabelValues(stateLabel(from)).Dec()
	m.watchers.WithLabelValues(stateLabel(to)).Inc()
}

func (m *metrics) removed(from watcherState, reason string) {
	m.watchers.WithLabelValues(stateLabel(from)).Dec()
	m.canceled.WithLabelValues(cancelLabel(reason)).Inc()
}

func (m *metrics) caughtUp(since time.Time) {
	m.catchUpDur.Observe(time.Since(since).Seconds())
}

func stateLabel(s watcherState) string {
	switch s {
	case stateSynced:
		return "synced"
	case stateUnsynced:
		return "unsynced"
	default:
		return "canceled"
	}
}

// cancelLabel maps a cancel reason to a metric label.
func cancelLabel(reason string) string {
	switch reason {
	case "":
		return "client"
	case reasonRewritten:
		return "rewritten"
	case reasonCompacted:
		return "compacted"
	case reasonClosed:
		return "closed"
	case reasonDeleted:
		return "deleted"
	case reasonReplaced:
		return "replaced"
	case reasonShutdown:
		return "shutdown"
	case ReasonResponseTooLarge:
		return "response_too_large"
	default:
		return "other"
	}
}
