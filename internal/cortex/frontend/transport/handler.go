// Copyright (c) The Cortex Authors.
// Licensed under the Apache License 2.0.

package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/httpgrpc/server"

	querier_stats "github.com/thanos-io/thanos/internal/cortex/querier/stats"
	"github.com/thanos-io/thanos/internal/cortex/tenant"
	"github.com/thanos-io/thanos/internal/cortex/util"
	util_log "github.com/thanos-io/thanos/internal/cortex/util/log"
)

const (
	// StatusClientClosedRequest is the status code for when a client request cancellation of an http request
	StatusClientClosedRequest = 499
	ServiceTimingHeaderName   = "Server-Timing"
)

var (
	errCanceled              = httpgrpc.Errorf(StatusClientClosedRequest, "%s", context.Canceled.Error())
	errDeadlineExceeded      = httpgrpc.Errorf(http.StatusGatewayTimeout, "%s", context.DeadlineExceeded.Error())
	errRequestEntityTooLarge = httpgrpc.Errorf(http.StatusRequestEntityTooLarge, "http: request body too large")
)

// Config for a Handler.
type HandlerConfig struct {
	LogQueriesLongerThan    time.Duration `yaml:"log_queries_longer_than"`
	MaxBodySize             int64         `yaml:"max_body_size"`
	QueryStatsEnabled       bool          `yaml:"query_stats_enabled"`
	SlowQueryLogsUserHeader string        `yaml:"slow_query_logs_user_header"`
}

// Handler accepts queries and forwards them to RoundTripper. It can log slow queries,
// but all other logic is inside the RoundTripper.
type Handler struct {
	cfg          HandlerConfig
	log          log.Logger
	roundTripper http.RoundTripper

	// Metrics.
	querySeconds *prometheus.CounterVec
	querySeries  *prometheus.CounterVec
	queryBytes   *prometheus.CounterVec
	activeUsers  *util.ActiveUsersCleanupService
}

// NewHandler creates a new frontend handler.
func NewHandler(cfg HandlerConfig, roundTripper http.RoundTripper, log log.Logger, reg prometheus.Registerer) http.Handler {
	h := &Handler{
		cfg:          cfg,
		log:          log,
		roundTripper: roundTripper,
	}

	if cfg.QueryStatsEnabled {
		h.querySeconds = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "cortex_query_seconds_total",
			Help: "Total amount of wall clock time spend processing queries.",
		}, []string{"user"})

		h.querySeries = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "cortex_query_fetched_series_total",
			Help: "Number of series fetched to execute a query.",
		}, []string{"user"})

		h.queryBytes = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "cortex_query_fetched_chunks_bytes_total",
			Help: "Size of all chunks fetched to execute a query in bytes.",
		}, []string{"user"})

		h.activeUsers = util.NewActiveUsersCleanupWithDefaultValues(func(user string) {
			h.querySeconds.DeleteLabelValues(user)
			h.querySeries.DeleteLabelValues(user)
			h.queryBytes.DeleteLabelValues(user)
		})
		// If cleaner stops or fail, we will simply not clean the metrics for inactive users.
		_ = h.activeUsers.StartAsync(context.Background())
	}

	return h
}

func (f *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var (
		stats       *querier_stats.Stats
		queryString url.Values
	)

	// Initialise the stats in the context and make sure it's propagated
	// down the request chain.
	if f.cfg.QueryStatsEnabled {
		var ctx context.Context
		stats, ctx = querier_stats.ContextWithEmptyStats(r.Context())
		r = r.WithContext(ctx)
	}

	defer func() {
		_ = r.Body.Close()
	}()

	// Buffer the body for later use to track slow queries.
	var buf bytes.Buffer
	r.Body = http.MaxBytesReader(w, r.Body, f.cfg.MaxBodySize)
	r.Body = io.NopCloser(io.TeeReader(r.Body, &buf))

	startTime := time.Now()
	resp, err := f.roundTripper.RoundTrip(r)
	queryResponseTime := time.Since(startTime)

	if err != nil {
		writeError(w, err)
		return
	}

	hs := w.Header()
	for h, vs := range resp.Header {
		hs[h] = vs
	}

	if f.cfg.QueryStatsEnabled {
		writeServiceTimingHeader(queryResponseTime, hs, stats)
	}

	w.WriteHeader(resp.StatusCode)
	// log copy response body error so that we will know even though success response code returned
	bytesCopied, err := io.Copy(w, resp.Body)
	if err != nil && !errors.Is(err, syscall.EPIPE) {
		level.Error(util_log.WithContext(r.Context(), f.log)).Log("msg", "write response body error", "bytesCopied", bytesCopied, "err", err)
	}

	// Check whether we should parse the query string.
	shouldReportSlowQuery := f.cfg.LogQueriesLongerThan != 0 &&
		queryResponseTime > f.cfg.LogQueriesLongerThan &&
		isQueryEndpoint(r.URL.Path)
	if shouldReportSlowQuery || f.cfg.QueryStatsEnabled {
		queryString = f.parseRequestQueryString(r, buf)
	}

	if shouldReportSlowQuery {
		f.reportSlowQuery(r, hs, queryString, queryResponseTime, stats)
	}
	if f.cfg.QueryStatsEnabled {
		f.reportQueryStats(r, queryString, queryResponseTime, stats)
	}
}

// isQueryEndpoint returns true if the path is any of the Prometheus HTTP API,
// query-related endpoints.
// Example: /api/v1/query, /api/v1/query_range, /api/v1/series, /api/v1/label, /api/v1/labels
func isQueryEndpoint(path string) bool {
	return strings.HasPrefix(path, "/api/v1")
}

// reportSlowQuery reports slow queries.
func (f *Handler) reportSlowQuery(
	r *http.Request,
	responseHeaders http.Header,
	queryString url.Values,
	queryResponseTime time.Duration,
	stats *querier_stats.Stats,
) {
	// NOTE(GiedriusS): see https://github.com/grafana/grafana/pull/60301 for more info.
	grafanaDashboardUID := "-"
	if dashboardUID := r.Header.Get("X-Dashboard-Uid"); dashboardUID != "" {
		grafanaDashboardUID = dashboardUID
	}
	grafanaPanelID := "-"
	if panelID := r.Header.Get("X-Panel-Id"); panelID != "" {
		grafanaPanelID = panelID
	}
	thanosTraceID := "-"
	if traceID := responseHeaders.Get("X-Thanos-Trace-Id"); traceID != "" {
		thanosTraceID = traceID
	}

	var remoteUser string
	// Prefer reading remote user from header. Fall back to the value of basic authentication.
	if f.cfg.SlowQueryLogsUserHeader != "" {
		remoteUser = r.Header.Get(f.cfg.SlowQueryLogsUserHeader)
	} else {
		remoteUser, _, _ = r.BasicAuth()
	}

	logMessage := append([]interface{}{
		"msg", "slow query detected",
		"method", r.Method,
		"host", r.Host,
		"path", r.URL.Path,
		"remote_user", remoteUser,
		"remote_addr", r.RemoteAddr,
		"time_taken", queryResponseTime.String(),
		"grafana_dashboard_uid", grafanaDashboardUID,
		"grafana_panel_id", grafanaPanelID,
		"trace_id", thanosTraceID,
	}, formatQueryString(queryString)...)

	logMessage = addQueryRangeToLogMessage(logMessage, queryString)
	logMessage = f.addStatsToLogMessage(logMessage, stats)

	level.Info(util_log.WithContext(r.Context(), f.log)).Log(logMessage...)
}

func (f *Handler) reportQueryStats(r *http.Request, queryString url.Values, queryResponseTime time.Duration, stats *querier_stats.Stats) {
	tenantIDs, err := tenant.TenantIDs(r.Context())
	if err != nil {
		return
	}
	userID := tenant.JoinTenantIDs(tenantIDs)
	wallTime := stats.LoadWallTime()
	numSeries := stats.LoadFetchedSeries()
	numBytes := stats.LoadFetchedChunkBytes()
	remoteUser, _, _ := r.BasicAuth()

	// Track stats.
	f.querySeconds.WithLabelValues(userID).Add(wallTime.Seconds())
	f.querySeries.WithLabelValues(userID).Add(float64(numSeries))
	f.queryBytes.WithLabelValues(userID).Add(float64(numBytes))
	f.activeUsers.UpdateUserTimestamp(userID, time.Now())

	// Log stats.
	logMessage := append([]interface{}{
		"msg", "query stats",
		"component", "query-frontend",
		"method", r.Method,
		"path", r.URL.Path,
		"remote_user", remoteUser,
		"remote_addr", r.RemoteAddr,
		"response_time", queryResponseTime,
		"query_wall_time_seconds", wallTime.Seconds(),
		"fetched_series_count", numSeries,
		"fetched_chunks_bytes", numBytes,
	}, formatQueryString(queryString)...)
	f.addStatsToLogMessage(logMessage, stats)
	addQueryRangeToLogMessage(logMessage, queryString)

	level.Info(util_log.WithContext(r.Context(), f.log)).Log(logMessage...)
}

func (f *Handler) parseRequestQueryString(r *http.Request, bodyBuf bytes.Buffer) url.Values {
	// Use previously buffered body.
	r.Body = io.NopCloser(&bodyBuf)

	// Ensure the form has been parsed so all the parameters are present
	err := r.ParseForm()
	if err != nil {
		level.Warn(util_log.WithContext(r.Context(), f.log)).Log("msg", "unable to parse request form", "err", err)
		return nil
	}

	return r.Form
}

func formatQueryString(queryString url.Values) (fields []interface{}) {
	for k, v := range queryString {
		fields = append(fields, fmt.Sprintf("param_%s", k), strings.Join(v, ","))
	}
	return fields
}

func (f *Handler) addStatsToLogMessage(message []interface{}, stats *querier_stats.Stats) []interface{} {
	if stats != nil {
		message = append(message, "peak_samples", stats.LoadPeakSamples())
		message = append(message, "total_samples_loaded", stats.LoadTotalSamples())
	}

	return message
}

func addQueryRangeToLogMessage(logMessage []interface{}, queryString url.Values) []interface{} {
	queryRange := extractQueryRange(queryString)
	if queryRange != time.Duration(0) {
		logMessage = append(logMessage, "query_range_hours", int(queryRange.Hours()))
		logMessage = append(logMessage, "query_range_human", queryRange.String())
	}
	return logMessage
}

// extractQueryRange extracts query range from query string.
// If start and end are not provided or are invalid, it returns a duration with zero-value.
func extractQueryRange(queryString url.Values) time.Duration {
	startStr := queryString.Get("start")
	endStr := queryString.Get("end")
	var queryRange = time.Duration(0)
	if startStr != "" && endStr != "" {
		start, serr := util.ParseTime(startStr)
		end, eerr := util.ParseTime(endStr)
		if serr == nil && eerr == nil {
			queryRange = time.Duration(end-start) * time.Millisecond
		}
	}
	return queryRange
}

func writeError(w http.ResponseWriter, err error) {
	switch err {
	case context.Canceled:
		err = errCanceled
	case context.DeadlineExceeded:
		err = errDeadlineExceeded
	default:
		if util.IsRequestBodyTooLarge(err) {
			err = errRequestEntityTooLarge
		}
	}
	server.WriteError(w, err)
}

func writeServiceTimingHeader(queryResponseTime time.Duration, headers http.Header, stats *querier_stats.Stats) {
	if stats != nil {
		parts := make([]string, 0)
		parts = append(parts, statsValue("querier_wall_time", stats.LoadWallTime()))
		parts = append(parts, statsValue("response_time", queryResponseTime))
		headers.Set(ServiceTimingHeaderName, strings.Join(parts, ", "))
	}
}

func statsValue(name string, d time.Duration) string {
	durationInMs := strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', -1, 64)
	return name + ";dur=" + durationInMs
}
