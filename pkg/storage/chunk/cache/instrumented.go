package cache

import (
	"context"

	instr "github.com/grafana/dskit/instrument"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	attribute "go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/loki/v3/pkg/util/constants"
)

// Instrument returns an instrumented cache.
func Instrument(name string, cache Cache, reg prometheus.Registerer) Cache {
	valueSize := promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
		Namespace: constants.Loki,
		Name:      "cache_value_size_bytes",
		Help:      "Size of values in the cache.",
		// Cached chunks are generally in the KBs, but cached index can
		// get big.  Histogram goes from 1KB to 4MB.
		// 1024 * 4^(7-1) = 4MB
		Buckets:     prometheus.ExponentialBuckets(1024, 4, 7),
		ConstLabels: prometheus.Labels{"name": name},
	}, []string{"method"})

	return &instrumentedCache{
		name:  name,
		Cache: cache,

		requestDuration: instr.NewHistogramCollector(promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
			Namespace: constants.Loki,
			Name:      "cache_request_duration_seconds",
			Help:      "Total time spent in seconds doing cache requests.",
			// Cache requests are very quick: smallest bucket is 16us, biggest is 1s.
			Buckets:     prometheus.ExponentialBuckets(0.000016, 4, 8),
			ConstLabels: prometheus.Labels{"name": name},
		}, []string{"method", "status_code"})),

		fetchedKeys: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Namespace:   constants.Loki,
			Name:        "cache_fetched_keys",
			Help:        "Total count of keys requested from cache.",
			ConstLabels: prometheus.Labels{"name": name},
		}),

		hits: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Namespace:   constants.Loki,
			Name:        "cache_hits",
			Help:        "Total count of keys found in cache.",
			ConstLabels: prometheus.Labels{"name": name},
		}),

		storedValueSize:  valueSize.WithLabelValues("store"),
		fetchedValueSize: valueSize.WithLabelValues("fetch"),
	}
}

type instrumentedCache struct {
	name string
	Cache

	fetchedKeys, hits                 prometheus.Counter
	storedValueSize, fetchedValueSize prometheus.Observer
	requestDuration                   *instr.HistogramCollector
}

func (i *instrumentedCache) Store(ctx context.Context, keys []string, bufs [][]byte) error {
	for j := range bufs {
		i.storedValueSize.Observe(float64(len(bufs[j])))
	}

	method := i.name + ".store"
	return instr.CollectedRequest(ctx, method, i.requestDuration, instr.ErrorCode, func(ctx context.Context) error {
		sp := trace.SpanFromContext(ctx)
		sp.SetAttributes(attribute.Int("keys", len(keys)))
		storeErr := i.Cache.Store(ctx, keys, bufs)
		if storeErr != nil {
			sp.SetStatus(codes.Error, storeErr.Error())
			sp.RecordError(storeErr)
		}
		return storeErr
	})
}

func (i *instrumentedCache) Fetch(ctx context.Context, keys []string) ([]string, [][]byte, []string, error) {
	var (
		found    []string
		bufs     [][]byte
		missing  []string
		fetchErr error
		method   = i.name + ".fetch"
	)

	err := instr.CollectedRequest(ctx, method, i.requestDuration, instr.ErrorCode, func(ctx context.Context) error {
		sp := trace.SpanFromContext(ctx)
		sp.SetAttributes(attribute.Int("keys requested", len(keys)))
		found, bufs, missing, fetchErr = i.Cache.Fetch(ctx, keys)
		if fetchErr != nil {
			sp.SetStatus(codes.Error, fetchErr.Error())
			sp.RecordError(fetchErr)
			return fetchErr
		}

		sp.SetAttributes(
			attribute.Int("keys found", len(found)),
			attribute.Int("keys missing", len(keys)-len(found)),
		)
		return nil
	})

	i.fetchedKeys.Add(float64(len(keys)))
	i.hits.Add(float64(len(found)))
	for j := range bufs {
		i.fetchedValueSize.Observe(float64(len(bufs[j])))
	}

	return found, bufs, missing, err
}

func (i *instrumentedCache) Stop() {
	i.Cache.Stop()
}
