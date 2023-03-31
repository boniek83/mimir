// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/queryrange/retry.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package querymiddleware

import (
	"context"
	"errors"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/weaveworks/common/httpgrpc"

	apierror "github.com/grafana/mimir/pkg/api/error"
	util_log "github.com/grafana/mimir/pkg/util/log"
)

type intObservation interface {
	Observe(int)
}

type retryMiddlewareMetrics struct {
	retriesCount prometheus.Histogram
}

func newRetryMiddlewareMetrics(registerer prometheus.Registerer) intObservation {
	return &retryMiddlewareMetrics{
		retriesCount: promauto.With(registerer).NewHistogram(prometheus.HistogramOpts{
			Namespace: "cortex",
			Name:      "query_frontend_retries",
			Help:      "Number of times a request is retried.",
			Buckets:   []float64{0, 1, 2, 3, 4, 5},
		}),
	}
}

func (m *retryMiddlewareMetrics) Observe(v int) {
	m.retriesCount.Observe(float64(v))
}

type retry struct {
	log        log.Logger
	next       Handler
	maxRetries int

	metrics intObservation
}

// newRetryMiddleware returns a middleware that retries requests if they
// fail with 500 or a non-HTTP error.
func newRetryMiddleware(log log.Logger, maxRetries int, metrics intObservation) Middleware {
	if metrics == nil {
		metrics = newRetryMiddlewareMetrics(nil)
	}

	return MiddlewareFunc(func(next Handler) Handler {
		return retry{
			log:        log,
			next:       next,
			maxRetries: maxRetries,
			metrics:    metrics,
		}
	})
}

func (r retry) Do(ctx context.Context, req Request) (Response, error) {
	tries := 0
	defer func() { r.metrics.Observe(tries) }()

	var lastErr error
	for ; tries < r.maxRetries; tries++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		resp, err := r.next.Do(ctx, req)
		if err == nil {
			return resp, nil
		}

		// Any error from the Prometheus API is non-retryable.
		if apierror.IsAPIError(err) || errors.Is(err, context.Canceled) {
			return nil, err
		}
		// Retry if we get a HTTP 500 or a non-HTTP error.
		httpResp, ok := httpgrpc.HTTPResponseFromError(err)
		if !ok || httpResp.Code/100 == 5 {
			lastErr = err
			level.Error(util_log.WithContext(ctx, r.log)).Log("msg", "error processing request", "try", tries, "err", err)
			continue
		}

		return nil, err
	}
	return nil, lastErr
}
