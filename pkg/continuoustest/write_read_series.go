// SPDX-License-Identifier: AGPL-3.0-only

package continuoustest

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"golang.org/x/time/rate"

	"github.com/grafana/dskit/multierror"

	"github.com/grafana/mimir/pkg/util/spanlogger"
)

const (
	writeInterval   = 20 * time.Second
	writeMaxAge     = 50 * time.Minute
	floatMetricName = "mimir_continuous_test_sine_wave"
	floatTypeLabel  = "float"
)

var (
	histogramMetricNames = []string{
		"mimir_continuous_test_histogram_int_counter",
		"mimir_continuous_test_histogram_float_counter",
		"mimir_continuous_test_histogram_int_gauge",
		"mimir_continuous_test_histogram_float_gauge",
	}
	histogramTypeLabels = []string{
		"histogram_int_counter",
		"histogram_float_counter",
		"histogram_int_gauge",
		"histogram_float_gauge",
	}
)

type querySumFunc func(metricName string) string

func querySumSample(metricName string) string {
	// We use max_over_time() with a 1s range selector in order to fetch only the samples we previously
	// wrote and ensure the PromQL lookback period doesn't influence query results. This help to avoid
	// false positives when finding the last written sample, or when restarting the testing tool with
	// a different number of configured series to write and read.
	return fmt.Sprintf("sum(max_over_time(%s[1s]))", metricName)
}

func querySumHist(metricName string) string {
	return fmt.Sprintf("sum(%s)", metricName)
}

type WriteReadSeriesTestConfig struct {
	NumSeries      int
	MaxQueryAge    time.Duration
	WithFloats     bool
	WithHistograms bool
}

func (cfg *WriteReadSeriesTestConfig) RegisterFlags(f *flag.FlagSet) {
	f.IntVar(&cfg.NumSeries, "tests.write-read-series-test.num-series", 10000, "Number of series used for the test.")
	f.DurationVar(&cfg.MaxQueryAge, "tests.write-read-series-test.max-query-age", 7*24*time.Hour, "How back in the past metrics can be queried at most.")
	f.BoolVar(&cfg.WithFloats, "tests.write-read-series-test.with-samples", false, "Include sample series in the tests?")
	f.BoolVar(&cfg.WithHistograms, "tests.write-read-series-test.with-histograms", false, "Include histogram series in the tests?")
}

type WriteReadSeriesTest struct {
	name    string
	cfg     WriteReadSeriesTestConfig
	client  MimirClient
	logger  log.Logger
	metrics *TestMetrics

	floatMetric MetricHistory
	histMetrics []MetricHistory
}

type MetricHistory struct {
	lastWrittenTimestamp time.Time
	queryMinTime         time.Time
	queryMaxTime         time.Time
}

func NewWriteReadSeriesTest(cfg WriteReadSeriesTestConfig, client MimirClient, logger log.Logger, reg prometheus.Registerer) *WriteReadSeriesTest {
	const name = "write-read-series"

	return &WriteReadSeriesTest{
		name:        name,
		cfg:         cfg,
		client:      client,
		logger:      log.With(logger, "test", name),
		metrics:     NewTestMetrics(name, reg),
		histMetrics: make([]MetricHistory, 4),
	}
}

// Name implements Test.
func (t *WriteReadSeriesTest) Name() string {
	return t.name
}

// Init implements Test.
func (t *WriteReadSeriesTest) Init(ctx context.Context, now time.Time) error {
	level.Info(t.logger).Log("msg", "Finding previously written samples time range to recover writes and reads from previous run")
	if t.cfg.WithFloats {
		err := t.recoverPast(ctx, now, floatMetricName, querySumSample, generateSineWaveValue, &t.floatMetric)
		if err != nil {
			return err
		}
	}
	if t.cfg.WithHistograms {
		for i := 0; i < 4; i++ {
			err := t.recoverPast(ctx, now, histogramMetricNames[i], querySumHist, generateHistogramValue[i], &t.histMetrics[i])
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *WriteReadSeriesTest) recoverPast(ctx context.Context, now time.Time, metricName string, querySum querySumFunc, generateValue generateValueFunc, records *MetricHistory) error {
	from, to := t.findPreviouslyWrittenTimeRange(ctx, now, metricName, querySum, generateValue)
	if from.IsZero() || to.IsZero() {
		level.Info(t.logger).Log("msg", "No valid previously written samples time range found, will continue writing from the nearest interval-aligned timestamp", "metric_name", metricName)
		return nil
	}
	if to.Before(now.Add(-writeMaxAge)) {
		level.Info(t.logger).Log("msg", "Previously written samples time range found but latest written sample is too old to recover", "metric_name", metricName, "last_sample_timestamp", to)
		return nil
	}

	records.lastWrittenTimestamp = to
	records.queryMinTime = from
	records.queryMaxTime = to
	level.Info(t.logger).Log("msg", "Successfully found previously written samples time range and recovered writes and reads from there", "metric_name", metricName, "last_written_timestamp", records.lastWrittenTimestamp, "query_min_time", records.queryMinTime, "query_max_time", records.queryMaxTime)

	return nil
}

// Run implements Test.
func (t *WriteReadSeriesTest) Run(ctx context.Context, now time.Time) error {
	// Configure the rate limiter to send a sample for each series per second. At startup, this test may catch up
	// with previous missing writes: this rate limit reduces the chances to hit the ingestion limit on Mimir side.
	writeLimiter := rate.NewLimiter(rate.Limit(t.cfg.NumSeries), t.cfg.NumSeries)

	// Collect all errors on this test run
	errs := new(multierror.MultiError)

	if t.cfg.WithFloats {
		t.RunInner(ctx, now, writeLimiter, errs, floatMetricName, floatTypeLabel, querySumSample, generateSineWaveSeries, generateSineWaveValue, &t.floatMetric)
	}

	if t.cfg.WithHistograms {
		for i := 0; i < 4; i++ {
			t.RunInner(ctx, now, writeLimiter, errs, histogramMetricNames[i], histogramTypeLabels[i], querySumHist, generateHistogramSeries[i], generateHistogramValue[i], &t.histMetrics[i])
		}
	}

	return errs.Err()
}

func (t *WriteReadSeriesTest) RunInner(ctx context.Context, now time.Time, writeLimiter *rate.Limiter, errs *multierror.MultiError, metricName, typeLabel string, querySum querySumFunc, generateSeries generateSeriesFunc, generateValue generateValueFunc, records *MetricHistory) {
	// Write series for each expected timestamp until now.
	for timestamp := t.nextWriteTimestamp(now, records); !timestamp.After(now); timestamp = t.nextWriteTimestamp(now, records) {
		if err := writeLimiter.WaitN(ctx, t.cfg.NumSeries); err != nil {
			// Context has been canceled, so we should interrupt.
			errs.Add(err)
			return
		}

		series := generateSeries(metricName, timestamp, t.cfg.NumSeries)
		if err := t.writeSamples(ctx, metricName, typeLabel, timestamp, series, records); err != nil {
			errs.Add(err)
			break
		}
	}

	queryRanges, queryInstants, err := t.getQueryTimeRanges(now, records)
	if err != nil {
		errs.Add(err)
	}

	queryMetric := querySum(metricName)
	for _, timeRange := range queryRanges {
		err := t.runRangeQueryAndVerifyResult(ctx, timeRange[0], timeRange[1], true, typeLabel, queryMetric, generateValue, records)
		errs.Add(err)
		err = t.runRangeQueryAndVerifyResult(ctx, timeRange[0], timeRange[1], false, typeLabel, queryMetric, generateValue, records)
		errs.Add(err)
	}
	for _, ts := range queryInstants {
		err := t.runInstantQueryAndVerifyResult(ctx, ts, true, typeLabel, queryMetric, generateValue, records)
		errs.Add(err)
		err = t.runInstantQueryAndVerifyResult(ctx, ts, false, typeLabel, queryMetric, generateValue, records)
		errs.Add(err)
	}
}

func (t *WriteReadSeriesTest) writeSamples(ctx context.Context, metricName, typeLabel string, timestamp time.Time, series []prompb.TimeSeries, records *MetricHistory) error {
	sp, ctx := spanlogger.NewWithLogger(ctx, t.logger, "WriteReadSeriesTest.writeSamples")
	defer sp.Finish()
	logger := log.With(sp, "timestamp", timestamp.String(), "num_series", t.cfg.NumSeries)

	statusCode, err := t.client.WriteSeries(ctx, series)

	t.metrics.writesTotal.WithLabelValues(typeLabel).Inc()
	if statusCode/100 != 2 {
		t.metrics.writesFailedTotal.WithLabelValues(strconv.Itoa(statusCode), typeLabel).Inc()
		level.Warn(logger).Log("msg", "Failed to remote write series", "status_code", statusCode, "err", err)
	} else {
		level.Debug(logger).Log("msg", "Remote write series succeeded")
	}

	// If the write request failed because of a 4xx error, retrying the request isn't expected to succeed.
	// The series may have been not written at all or partially written (eg. we hit some limit).
	// We keep writing the next interval, but we reset the query timestamp because we can't reliably
	// assert on query results due to possible gaps.
	if statusCode/100 == 4 {
		records.lastWrittenTimestamp = timestamp
		records.queryMinTime = time.Time{}
		records.queryMaxTime = time.Time{}
		return nil
	}

	// If the write request failed because of a network or 5xx error, we'll retry to write series
	// in the next test run.
	if err != nil {
		return errors.Wrap(err, "failed to remote write series")
	}
	if statusCode/100 != 2 {
		return errors.Wrapf(err, "remote write series failed with status code %d", statusCode)
	}

	// The write request succeeded.
	records.lastWrittenTimestamp = timestamp
	records.queryMaxTime = timestamp
	if records.queryMinTime.IsZero() {
		records.queryMinTime = timestamp
	}

	return nil
}

// getQueryTimeRanges returns the start/end time ranges to use to run test range queries,
// and the timestamps to use to run test instant queries.
func (t *WriteReadSeriesTest) getQueryTimeRanges(now time.Time, records *MetricHistory) (ranges [][2]time.Time, instants []time.Time, err error) {
	// The min and max allowed query timestamps are zero if there's no successfully written data yet.
	if records.queryMinTime.IsZero() || records.queryMaxTime.IsZero() {
		level.Info(t.logger).Log("msg", "Skipped queries because there's no valid time range to query")
		return nil, nil, errors.New("no valid time range to query")
	}

	// Honor the configured max age.
	adjustedQueryMinTime := maxTime(records.queryMinTime, now.Add(-t.cfg.MaxQueryAge))
	if records.queryMaxTime.Before(adjustedQueryMinTime) {
		level.Info(t.logger).Log("msg", "Skipped queries because there's no valid time range to query after honoring configured max query age", "min_valid_time", records.queryMinTime, "max_valid_time", records.queryMaxTime, "max_query_age", t.cfg.MaxQueryAge)
		return nil, nil, errors.New("no valid time range to query after honoring configured max query age")
	}

	// Last 1h.
	if records.queryMaxTime.After(now.Add(-1 * time.Hour)) {
		ranges = append(ranges, [2]time.Time{
			maxTime(adjustedQueryMinTime, now.Add(-1*time.Hour)),
			minTime(records.queryMaxTime, now),
		})
		instants = append(instants, minTime(records.queryMaxTime, now))
	}

	// Last 24h (only if the actual time range is not already covered by "Last 1h").
	if records.queryMaxTime.After(now.Add(-24*time.Hour)) && adjustedQueryMinTime.Before(now.Add(-1*time.Hour)) {
		ranges = append(ranges, [2]time.Time{
			maxTime(adjustedQueryMinTime, now.Add(-24*time.Hour)),
			minTime(records.queryMaxTime, now),
		})
		instants = append(instants, maxTime(adjustedQueryMinTime, now.Add(-24*time.Hour)))
	}

	// From last 23h to last 24h.
	if adjustedQueryMinTime.Before(now.Add(-23*time.Hour)) && records.queryMaxTime.After(now.Add(-23*time.Hour)) {
		ranges = append(ranges, [2]time.Time{
			maxTime(adjustedQueryMinTime, now.Add(-24*time.Hour)),
			minTime(records.queryMaxTime, now.Add(-23*time.Hour)),
		})
	}

	// A random time range.
	randMinTime := randTime(adjustedQueryMinTime, records.queryMaxTime)
	ranges = append(ranges, [2]time.Time{randMinTime, randTime(randMinTime, records.queryMaxTime)})
	instants = append(instants, randMinTime)

	return ranges, instants, nil
}

func (t *WriteReadSeriesTest) runRangeQueryAndVerifyResult(ctx context.Context, start, end time.Time, resultsCacheEnabled bool, typeLabel, metricSumQuery string, generateValue generateValueFunc, records *MetricHistory) error {
	// We align start, end and step to write interval in order to avoid any false positives
	// when checking results correctness. The min/max query time is always aligned.
	start = maxTime(records.queryMinTime, alignTimestampToInterval(start, writeInterval))
	end = minTime(records.queryMaxTime, alignTimestampToInterval(end, writeInterval))
	if end.Before(start) {
		return nil
	}

	step := getQueryStep(start, end, writeInterval)

	sp, ctx := spanlogger.NewWithLogger(ctx, t.logger, "WriteReadSeriesTest.runRangeQueryAndVerifyResult")
	defer sp.Finish()

	logger := log.With(sp, "query", metricSumQuery, "start", start.UnixMilli(), "end", end.UnixMilli(), "step", step, "results_cache", strconv.FormatBool(resultsCacheEnabled))
	level.Debug(logger).Log("msg", "Running range query")

	t.metrics.queriesTotal.WithLabelValues(typeLabel).Inc()
	matrix, err := t.client.QueryRange(ctx, metricSumQuery, start, end, step, WithResultsCacheEnabled(resultsCacheEnabled))
	if err != nil {
		t.metrics.queriesFailedTotal.WithLabelValues(typeLabel).Inc()
		level.Warn(logger).Log("msg", "Failed to execute range query", "err", err)
		return errors.Wrap(err, "failed to execute range query")
	}

	t.metrics.queryResultChecksTotal.WithLabelValues(typeLabel).Inc()
	_, err = verifySamplesSum(matrix, t.cfg.NumSeries, step, generateValue)
	if err != nil {
		t.metrics.queryResultChecksFailedTotal.WithLabelValues(typeLabel).Inc()
		level.Warn(logger).Log("msg", "Range query result check failed", "err", err)
		return errors.Wrap(err, "range query result check failed")
	}
	return nil
}

func (t *WriteReadSeriesTest) runInstantQueryAndVerifyResult(ctx context.Context, ts time.Time, resultsCacheEnabled bool, typeLabel, metricSumQuery string, generateValue generateValueFunc, records *MetricHistory) error {
	// We align the query timestamp to write interval in order to avoid any false positives
	// when checking results correctness. The min/max query time is always aligned.
	ts = maxTime(records.queryMinTime, alignTimestampToInterval(ts, writeInterval))
	if records.queryMaxTime.Before(ts) {
		return nil
	}

	sp, ctx := spanlogger.NewWithLogger(ctx, t.logger, "WriteReadSeriesTest.runInstantQueryAndVerifyResult")
	defer sp.Finish()

	logger := log.With(sp, "query", metricSumQuery, "ts", ts.UnixMilli(), "results_cache", strconv.FormatBool(resultsCacheEnabled))
	level.Debug(logger).Log("msg", "Running instant query")

	t.metrics.queriesTotal.WithLabelValues(typeLabel).Inc()
	vector, err := t.client.Query(ctx, metricSumQuery, ts, WithResultsCacheEnabled(resultsCacheEnabled))
	if err != nil {
		t.metrics.queriesFailedTotal.WithLabelValues(typeLabel).Inc()
		level.Warn(logger).Log("msg", "Failed to execute instant query", "err", err)
		return errors.Wrap(err, "failed to execute instant query")
	}

	// Convert the vector to matrix to reuse the same results comparison utility.
	matrix := make(model.Matrix, 0, len(vector))
	for _, entry := range vector {
		ss := &model.SampleStream{
			Metric: entry.Metric,
		}
		if entry.Histogram == nil {
			ss.Values = []model.SamplePair{{
				Timestamp: entry.Timestamp,
				Value:     entry.Value,
			}}
		} else {
			ss.Histograms = []model.SampleHistogramPair{{
				Timestamp: entry.Timestamp,
				Histogram: entry.Histogram,
			}}
		}
		matrix = append(matrix, ss)
	}

	t.metrics.queryResultChecksTotal.WithLabelValues(typeLabel).Inc()
	_, err = verifySamplesSum(matrix, t.cfg.NumSeries, 0, generateValue)
	if err != nil {
		t.metrics.queryResultChecksFailedTotal.WithLabelValues(typeLabel).Inc()
		level.Warn(logger).Log("msg", "Instant query result check failed", "err", err)
		return errors.Wrap(err, "instant query result check failed")
	}
	return nil
}

func (t *WriteReadSeriesTest) nextWriteTimestamp(now time.Time, records *MetricHistory) time.Time {
	if records.lastWrittenTimestamp.IsZero() {
		return alignTimestampToInterval(now, writeInterval)
	}

	return records.lastWrittenTimestamp.Add(writeInterval)
}

func (t *WriteReadSeriesTest) findPreviouslyWrittenTimeRange(ctx context.Context, now time.Time, metricName string, querySum querySumFunc, generateValue generateValueFunc) (from, to time.Time) {
	end := alignTimestampToInterval(now, writeInterval)
	step := writeInterval

	var samples []model.SamplePair
	var histograms []model.SampleHistogramPair
	query := querySum(metricName)

	for {
		start := alignTimestampToInterval(maxTime(now.Add(-t.cfg.MaxQueryAge), end.Add(-24*time.Hour).Add(step)), writeInterval)
		if !start.Before(end) {
			// We've hit the max query age, so we'll keep the last computed valid time range (if any).
			return
		}

		logger := log.With(t.logger, "query", query, "start", start, "end", end, "step", step)
		level.Debug(logger).Log("msg", "Executing query to find previously written samples", "metric_name", metricName)

		matrix, err := t.client.QueryRange(ctx, query, start, end, step, WithResultsCacheEnabled(false))
		if err != nil {
			level.Warn(logger).Log("msg", "Failed to execute range query used to find previously written samples", "query", query, "err", err)
			return
		}

		if len(matrix) == 0 {
			// No samples found, so we'll keep the last computed valid time range (if any).
			return
		}

		if len(matrix) != 1 {
			level.Error(logger).Log("msg", "The range query used to find previously written samples returned an unexpected number of series", "query", query, "expected", 1, "returned", len(matrix))
			return
		}

		samples = append(matrix[0].Values, samples...)
		histograms = append(matrix[0].Histograms, histograms...)
		end = start.Add(-step)

		var fullMatrix model.Matrix
		useHistograms := false
		if len(samples) > 0 && len(histograms) == 0 {
			fullMatrix = model.Matrix{{Values: samples}}
		} else if len(histograms) > 0 && len(samples) == 0 {
			fullMatrix = model.Matrix{{Histograms: histograms}}
			useHistograms = true
		} else {
			level.Error(logger).Log("msg", "The range query used to find previously written samples returned either both floats and histograms or neither", "query", query)
			return
		}
		lastMatchingIdx, _ := verifySamplesSum(fullMatrix, t.cfg.NumSeries, step, generateValue)
		if lastMatchingIdx == -1 {
			return
		}

		// Update the previously written time range.
		if useHistograms {
			from = histograms[lastMatchingIdx].Timestamp.Time()
			to = histograms[len(histograms)-1].Timestamp.Time()
		} else {
			from = samples[lastMatchingIdx].Timestamp.Time()
			to = samples[len(samples)-1].Timestamp.Time()
		}

		// If the last matching sample is not the one at the beginning of the queried time range
		// then it means we've found the oldest previously written sample and we can stop searching it.
		if lastMatchingIdx != 0 || (!useHistograms && !samples[0].Timestamp.Time().Equal(start)) || (useHistograms && !histograms[0].Timestamp.Time().Equal(start)) {
			return
		}
	}
}
