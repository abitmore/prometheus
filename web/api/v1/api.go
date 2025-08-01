// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/regexp"
	jsoniter "github.com/json-iterator/go"
	"github.com/munnerz/goautoneg"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/route"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/scrape"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/index"
	"github.com/prometheus/prometheus/util/annotations"
	"github.com/prometheus/prometheus/util/httputil"
	"github.com/prometheus/prometheus/util/notifications"
	"github.com/prometheus/prometheus/util/stats"
)

type status string

const (
	statusSuccess status = "success"
	statusError   status = "error"

	// Non-standard status code (originally introduced by nginx) for the case when a client closes
	// the connection while the server is still processing the request.
	statusClientClosedConnection = 499

	// checkContextEveryNIterations is used in some tight loops to check if the context is done.
	checkContextEveryNIterations = 128
)

type errorType string

const (
	errorNone          errorType = ""
	errorTimeout       errorType = "timeout"
	errorCanceled      errorType = "canceled"
	errorExec          errorType = "execution"
	errorBadData       errorType = "bad_data"
	errorInternal      errorType = "internal"
	errorUnavailable   errorType = "unavailable"
	errorNotFound      errorType = "not_found"
	errorNotAcceptable errorType = "not_acceptable"
)

var LocalhostRepresentations = []string{"127.0.0.1", "localhost", "::1"}

type apiError struct {
	typ errorType
	err error
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s: %s", e.typ, e.err)
}

// ScrapePoolsRetriever provide the list of all scrape pools.
type ScrapePoolsRetriever interface {
	ScrapePools() []string
}

// TargetRetriever provides the list of active/dropped targets to scrape or not.
type TargetRetriever interface {
	TargetsActive() map[string][]*scrape.Target
	TargetsDropped() map[string][]*scrape.Target
	TargetsDroppedCounts() map[string]int
}

// AlertmanagerRetriever provides a list of all/dropped AlertManager URLs.
type AlertmanagerRetriever interface {
	Alertmanagers() []*url.URL
	DroppedAlertmanagers() []*url.URL
}

// RulesRetriever provides a list of active rules and alerts.
type RulesRetriever interface {
	RuleGroups() []*rules.Group
	AlertingRules() []*rules.AlertingRule
}

// StatsRenderer converts engine statistics into a format suitable for the API.
type StatsRenderer func(context.Context, *stats.Statistics, string) stats.QueryStats

// DefaultStatsRenderer is the default stats renderer for the API.
func DefaultStatsRenderer(_ context.Context, s *stats.Statistics, param string) stats.QueryStats {
	if param != "" {
		return stats.NewQueryStats(s)
	}
	return nil
}

// PrometheusVersion contains build information about Prometheus.
type PrometheusVersion struct {
	Version   string `json:"version"`
	Revision  string `json:"revision"`
	Branch    string `json:"branch"`
	BuildUser string `json:"buildUser"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
}

// RuntimeInfo contains runtime information about Prometheus.
type RuntimeInfo struct {
	StartTime           time.Time `json:"startTime"`
	CWD                 string    `json:"CWD"`
	Hostname            string    `json:"hostname"`
	ServerTime          time.Time `json:"serverTime"`
	ReloadConfigSuccess bool      `json:"reloadConfigSuccess"`
	LastConfigTime      time.Time `json:"lastConfigTime"`
	CorruptionCount     int64     `json:"corruptionCount"`
	GoroutineCount      int       `json:"goroutineCount"`
	GOMAXPROCS          int       `json:"GOMAXPROCS"`
	GOMEMLIMIT          int64     `json:"GOMEMLIMIT"`
	GOGC                string    `json:"GOGC"`
	GODEBUG             string    `json:"GODEBUG"`
	StorageRetention    string    `json:"storageRetention"`
}

// Response contains a response to a HTTP API request.
type Response struct {
	Status    status      `json:"status"`
	Data      interface{} `json:"data,omitempty"`
	ErrorType errorType   `json:"errorType,omitempty"`
	Error     string      `json:"error,omitempty"`
	Warnings  []string    `json:"warnings,omitempty"`
	Infos     []string    `json:"infos,omitempty"`
}

type apiFuncResult struct {
	data      interface{}
	err       *apiError
	warnings  annotations.Annotations
	finalizer func()
}

type apiFunc func(r *http.Request) apiFuncResult

// TSDBAdminStats defines the tsdb interfaces used by the v1 API for admin operations as well as statistics.
type TSDBAdminStats interface {
	CleanTombstones() error
	Delete(ctx context.Context, mint, maxt int64, ms ...*labels.Matcher) error
	Snapshot(dir string, withHead bool) error
	Stats(statsByLabelName string, limit int) (*tsdb.Stats, error)
	WALReplayStatus() (tsdb.WALReplayStatus, error)
	BlockMetas() ([]tsdb.BlockMeta, error)
}

type QueryOpts interface {
	EnablePerStepStats() bool
	LookbackDelta() time.Duration
}

// API can register a set of endpoints in a router and handle
// them using the provided storage and query engine.
type API struct {
	Queryable         storage.SampleAndChunkQueryable
	QueryEngine       promql.QueryEngine
	ExemplarQueryable storage.ExemplarQueryable

	scrapePoolsRetriever  func(context.Context) ScrapePoolsRetriever
	targetRetriever       func(context.Context) TargetRetriever
	alertmanagerRetriever func(context.Context) AlertmanagerRetriever
	rulesRetriever        func(context.Context) RulesRetriever
	now                   func() time.Time
	config                func() config.Config
	flagsMap              map[string]string
	ready                 func(http.HandlerFunc) http.HandlerFunc
	globalURLOptions      GlobalURLOptions

	db                  TSDBAdminStats
	dbDir               string
	enableAdmin         bool
	logger              *slog.Logger
	CORSOrigin          *regexp.Regexp
	buildInfo           *PrometheusVersion
	runtimeInfo         func() (RuntimeInfo, error)
	gatherer            prometheus.Gatherer
	isAgent             bool
	statsRenderer       StatsRenderer
	notificationsGetter func() []notifications.Notification
	notificationsSub    func() (<-chan notifications.Notification, func(), bool)

	remoteWriteHandler http.Handler
	remoteReadHandler  http.Handler
	otlpWriteHandler   http.Handler

	codecs []Codec
}

// NewAPI returns an initialized API type.
func NewAPI(
	qe promql.QueryEngine,
	q storage.SampleAndChunkQueryable,
	ap storage.Appendable,
	eq storage.ExemplarQueryable,
	spsr func(context.Context) ScrapePoolsRetriever,
	tr func(context.Context) TargetRetriever,
	ar func(context.Context) AlertmanagerRetriever,
	configFunc func() config.Config,
	flagsMap map[string]string,
	globalURLOptions GlobalURLOptions,
	readyFunc func(http.HandlerFunc) http.HandlerFunc,
	db TSDBAdminStats,
	dbDir string,
	enableAdmin bool,
	logger *slog.Logger,
	rr func(context.Context) RulesRetriever,
	remoteReadSampleLimit int,
	remoteReadConcurrencyLimit int,
	remoteReadMaxBytesInFrame int,
	isAgent bool,
	corsOrigin *regexp.Regexp,
	runtimeInfo func() (RuntimeInfo, error),
	buildInfo *PrometheusVersion,
	notificationsGetter func() []notifications.Notification,
	notificationsSub func() (<-chan notifications.Notification, func(), bool),
	gatherer prometheus.Gatherer,
	registerer prometheus.Registerer,
	statsRenderer StatsRenderer,
	rwEnabled bool,
	acceptRemoteWriteProtoMsgs []config.RemoteWriteProtoMsg,
	otlpEnabled, otlpDeltaToCumulative, otlpNativeDeltaIngestion bool,
	ctZeroIngestionEnabled bool,
	lookbackDelta time.Duration,
	enableTypeAndUnitLabels bool,
) *API {
	a := &API{
		QueryEngine:       qe,
		Queryable:         q,
		ExemplarQueryable: eq,

		scrapePoolsRetriever:  spsr,
		targetRetriever:       tr,
		alertmanagerRetriever: ar,

		now:                 time.Now,
		config:              configFunc,
		flagsMap:            flagsMap,
		ready:               readyFunc,
		globalURLOptions:    globalURLOptions,
		db:                  db,
		dbDir:               dbDir,
		enableAdmin:         enableAdmin,
		rulesRetriever:      rr,
		logger:              logger,
		CORSOrigin:          corsOrigin,
		runtimeInfo:         runtimeInfo,
		buildInfo:           buildInfo,
		gatherer:            gatherer,
		isAgent:             isAgent,
		statsRenderer:       DefaultStatsRenderer,
		notificationsGetter: notificationsGetter,
		notificationsSub:    notificationsSub,

		remoteReadHandler: remote.NewReadHandler(logger, registerer, q, configFunc, remoteReadSampleLimit, remoteReadConcurrencyLimit, remoteReadMaxBytesInFrame),
	}

	a.InstallCodec(JSONCodec{})

	if statsRenderer != nil {
		a.statsRenderer = statsRenderer
	}

	if ap == nil && (rwEnabled || otlpEnabled) {
		panic("remote write or otlp write enabled, but no appender passed in.")
	}

	if rwEnabled {
		a.remoteWriteHandler = remote.NewWriteHandler(logger, registerer, ap, acceptRemoteWriteProtoMsgs, ctZeroIngestionEnabled)
	}
	if otlpEnabled {
		a.otlpWriteHandler = remote.NewOTLPWriteHandler(logger, registerer, ap, configFunc, remote.OTLPOptions{
			ConvertDelta:            otlpDeltaToCumulative,
			NativeDelta:             otlpNativeDeltaIngestion,
			LookbackDelta:           lookbackDelta,
			EnableTypeAndUnitLabels: enableTypeAndUnitLabels,
		})
	}

	return a
}

// InstallCodec adds codec to this API's available codecs.
// Codecs installed first take precedence over codecs installed later when evaluating wildcards in Accept headers.
// The first installed codec is used as a fallback when the Accept header cannot be satisfied or if there is no Accept header.
func (api *API) InstallCodec(codec Codec) {
	api.codecs = append(api.codecs, codec)
}

// ClearCodecs removes all available codecs from this API, including the default codec installed by NewAPI.
func (api *API) ClearCodecs() {
	api.codecs = nil
}

func setUnavailStatusOnTSDBNotReady(r apiFuncResult) apiFuncResult {
	if r.err != nil && errors.Is(r.err.err, tsdb.ErrNotReady) {
		r.err.typ = errorUnavailable
	}
	return r
}

// Register the API's endpoints in the given router.
func (api *API) Register(r *route.Router) {
	wrap := func(f apiFunc) http.HandlerFunc {
		hf := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httputil.SetCORS(w, api.CORSOrigin, r)
			result := setUnavailStatusOnTSDBNotReady(f(r))
			if result.finalizer != nil {
				defer result.finalizer()
			}
			if result.err != nil {
				api.respondError(w, result.err, result.data)
				return
			}

			if result.data != nil {
				api.respond(w, r, result.data, result.warnings, r.FormValue("query"))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		return api.ready(httputil.CompressionHandler{
			Handler: hf,
		}.ServeHTTP)
	}

	wrapAgent := func(f apiFunc) http.HandlerFunc {
		return wrap(func(r *http.Request) apiFuncResult {
			if api.isAgent {
				return apiFuncResult{nil, &apiError{errorExec, errors.New("unavailable with Prometheus Agent")}, nil, nil}
			}
			return f(r)
		})
	}

	r.Options("/*path", wrap(api.options))

	r.Get("/query", wrapAgent(api.query))
	r.Post("/query", wrapAgent(api.query))
	r.Get("/query_range", wrapAgent(api.queryRange))
	r.Post("/query_range", wrapAgent(api.queryRange))
	r.Get("/query_exemplars", wrapAgent(api.queryExemplars))
	r.Post("/query_exemplars", wrapAgent(api.queryExemplars))

	r.Get("/format_query", wrapAgent(api.formatQuery))
	r.Post("/format_query", wrapAgent(api.formatQuery))

	r.Get("/parse_query", wrapAgent(api.parseQuery))
	r.Post("/parse_query", wrapAgent(api.parseQuery))

	r.Get("/labels", wrapAgent(api.labelNames))
	r.Post("/labels", wrapAgent(api.labelNames))
	r.Get("/label/:name/values", wrapAgent(api.labelValues))

	r.Get("/series", wrapAgent(api.series))
	r.Post("/series", wrapAgent(api.series))
	r.Del("/series", wrapAgent(api.dropSeries))

	r.Get("/scrape_pools", wrap(api.scrapePools))
	r.Get("/targets", wrap(api.targets))
	r.Get("/targets/metadata", wrap(api.targetMetadata))
	r.Get("/alertmanagers", wrapAgent(api.alertmanagers))

	r.Get("/metadata", wrap(api.metricMetadata))

	r.Get("/status/config", wrap(api.serveConfig))
	r.Get("/status/runtimeinfo", wrap(api.serveRuntimeInfo))
	r.Get("/status/buildinfo", wrap(api.serveBuildInfo))
	r.Get("/status/flags", wrap(api.serveFlags))
	r.Get("/status/tsdb", wrapAgent(api.serveTSDBStatus))
	r.Get("/status/tsdb/blocks", wrapAgent(api.serveTSDBBlocks))
	r.Get("/status/walreplay", api.serveWALReplayStatus)
	r.Get("/notifications", api.notifications)
	r.Get("/notifications/live", api.notificationsSSE)
	r.Post("/read", api.ready(api.remoteRead))
	r.Post("/write", api.ready(api.remoteWrite))
	r.Post("/otlp/v1/metrics", api.ready(api.otlpWrite))

	r.Get("/alerts", wrapAgent(api.alerts))
	r.Get("/rules", wrapAgent(api.rules))

	// Admin APIs
	r.Post("/admin/tsdb/delete_series", wrapAgent(api.deleteSeries))
	r.Post("/admin/tsdb/clean_tombstones", wrapAgent(api.cleanTombstones))
	r.Post("/admin/tsdb/snapshot", wrapAgent(api.snapshot))

	r.Put("/admin/tsdb/delete_series", wrapAgent(api.deleteSeries))
	r.Put("/admin/tsdb/clean_tombstones", wrapAgent(api.cleanTombstones))
	r.Put("/admin/tsdb/snapshot", wrapAgent(api.snapshot))
}

type QueryData struct {
	ResultType parser.ValueType `json:"resultType"`
	Result     parser.Value     `json:"result"`
	Stats      stats.QueryStats `json:"stats,omitempty"`
}

func invalidParamError(err error, parameter string) apiFuncResult {
	return apiFuncResult{nil, &apiError{
		errorBadData, fmt.Errorf("invalid parameter %q: %w", parameter, err),
	}, nil, nil}
}

func (api *API) options(*http.Request) apiFuncResult {
	return apiFuncResult{nil, nil, nil, nil}
}

func (api *API) query(r *http.Request) (result apiFuncResult) {
	limit, err := parseLimitParam(r.FormValue("limit"))
	if err != nil {
		return invalidParamError(err, "limit")
	}
	ts, err := parseTimeParam(r, "time", api.now())
	if err != nil {
		return invalidParamError(err, "time")
	}
	ctx := r.Context()
	if to := r.FormValue("timeout"); to != "" {
		var cancel context.CancelFunc
		timeout, err := parseDuration(to)
		if err != nil {
			return invalidParamError(err, "timeout")
		}

		ctx, cancel = context.WithDeadline(ctx, api.now().Add(timeout))
		defer cancel()
	}

	opts, err := extractQueryOpts(r)
	if err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, err}, nil, nil}
	}
	qry, err := api.QueryEngine.NewInstantQuery(ctx, api.Queryable, opts, r.FormValue("query"), ts)
	if err != nil {
		return invalidParamError(err, "query")
	}

	// From now on, we must only return with a finalizer in the result (to
	// be called by the caller) or call qry.Close ourselves (which is
	// required in the case of a panic).
	defer func() {
		if result.finalizer == nil {
			qry.Close()
		}
	}()

	ctx = httputil.ContextFromRequest(ctx, r)

	res := qry.Exec(ctx)
	if res.Err != nil {
		return apiFuncResult{nil, returnAPIError(res.Err), res.Warnings, qry.Close}
	}

	warnings := res.Warnings
	if limit > 0 {
		var isTruncated bool

		res, isTruncated = truncateResults(res, limit)
		if isTruncated {
			warnings = warnings.Add(errors.New("results truncated due to limit"))
		}
	}
	// Optional stats field in response if parameter "stats" is not empty.
	sr := api.statsRenderer
	if sr == nil {
		sr = DefaultStatsRenderer
	}
	qs := sr(ctx, qry.Stats(), r.FormValue("stats"))

	return apiFuncResult{&QueryData{
		ResultType: res.Value.Type(),
		Result:     res.Value,
		Stats:      qs,
	}, nil, warnings, qry.Close}
}

func (api *API) formatQuery(r *http.Request) (result apiFuncResult) {
	expr, err := parser.ParseExpr(r.FormValue("query"))
	if err != nil {
		return invalidParamError(err, "query")
	}

	return apiFuncResult{expr.Pretty(0), nil, nil, nil}
}

func (api *API) parseQuery(r *http.Request) apiFuncResult {
	expr, err := parser.ParseExpr(r.FormValue("query"))
	if err != nil {
		return invalidParamError(err, "query")
	}

	return apiFuncResult{data: translateAST(expr), err: nil, warnings: nil, finalizer: nil}
}

func extractQueryOpts(r *http.Request) (promql.QueryOpts, error) {
	var duration time.Duration

	if strDuration := r.FormValue("lookback_delta"); strDuration != "" {
		parsedDuration, err := parseDuration(strDuration)
		if err != nil {
			return nil, fmt.Errorf("error parsing lookback delta duration: %w", err)
		}
		duration = parsedDuration
	}

	return promql.NewPrometheusQueryOpts(r.FormValue("stats") == "all", duration), nil
}

func (api *API) queryRange(r *http.Request) (result apiFuncResult) {
	limit, err := parseLimitParam(r.FormValue("limit"))
	if err != nil {
		return invalidParamError(err, "limit")
	}
	start, err := parseTime(r.FormValue("start"))
	if err != nil {
		return invalidParamError(err, "start")
	}
	end, err := parseTime(r.FormValue("end"))
	if err != nil {
		return invalidParamError(err, "end")
	}
	if end.Before(start) {
		return invalidParamError(errors.New("end timestamp must not be before start time"), "end")
	}

	step, err := parseDuration(r.FormValue("step"))
	if err != nil {
		return invalidParamError(err, "step")
	}

	if step <= 0 {
		return invalidParamError(errors.New("zero or negative query resolution step widths are not accepted. Try a positive integer"), "step")
	}

	// For safety, limit the number of returned points per timeseries.
	// This is sufficient for 60s resolution for a week or 1h resolution for a year.
	if end.Sub(start)/step > 11000 {
		err := errors.New("exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)")
		return apiFuncResult{nil, &apiError{errorBadData, err}, nil, nil}
	}

	ctx := r.Context()
	if to := r.FormValue("timeout"); to != "" {
		var cancel context.CancelFunc
		timeout, err := parseDuration(to)
		if err != nil {
			return invalidParamError(err, "timeout")
		}

		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	opts, err := extractQueryOpts(r)
	if err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, err}, nil, nil}
	}
	qry, err := api.QueryEngine.NewRangeQuery(ctx, api.Queryable, opts, r.FormValue("query"), start, end, step)
	if err != nil {
		return invalidParamError(err, "query")
	}
	// From now on, we must only return with a finalizer in the result (to
	// be called by the caller) or call qry.Close ourselves (which is
	// required in the case of a panic).
	defer func() {
		if result.finalizer == nil {
			qry.Close()
		}
	}()

	ctx = httputil.ContextFromRequest(ctx, r)

	res := qry.Exec(ctx)
	if res.Err != nil {
		return apiFuncResult{nil, returnAPIError(res.Err), res.Warnings, qry.Close}
	}

	warnings := res.Warnings
	if limit > 0 {
		var isTruncated bool

		res, isTruncated = truncateResults(res, limit)
		if isTruncated {
			warnings = warnings.Add(errors.New("results truncated due to limit"))
		}
	}

	// Optional stats field in response if parameter "stats" is not empty.
	sr := api.statsRenderer
	if sr == nil {
		sr = DefaultStatsRenderer
	}
	qs := sr(ctx, qry.Stats(), r.FormValue("stats"))

	return apiFuncResult{&QueryData{
		ResultType: res.Value.Type(),
		Result:     res.Value,
		Stats:      qs,
	}, nil, warnings, qry.Close}
}

func (api *API) queryExemplars(r *http.Request) apiFuncResult {
	start, err := parseTimeParam(r, "start", MinTime)
	if err != nil {
		return invalidParamError(err, "start")
	}
	end, err := parseTimeParam(r, "end", MaxTime)
	if err != nil {
		return invalidParamError(err, "end")
	}
	if end.Before(start) {
		err := errors.New("end timestamp must not be before start timestamp")
		return apiFuncResult{nil, &apiError{errorBadData, err}, nil, nil}
	}

	expr, err := parser.ParseExpr(r.FormValue("query"))
	if err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, err}, nil, nil}
	}

	selectors := parser.ExtractSelectors(expr)
	if len(selectors) < 1 {
		return apiFuncResult{nil, nil, nil, nil}
	}

	ctx := r.Context()
	eq, err := api.ExemplarQueryable.ExemplarQuerier(ctx)
	if err != nil {
		return apiFuncResult{nil, returnAPIError(err), nil, nil}
	}

	res, err := eq.Select(timestamp.FromTime(start), timestamp.FromTime(end), selectors...)
	if err != nil {
		return apiFuncResult{nil, returnAPIError(err), nil, nil}
	}

	return apiFuncResult{res, nil, nil, nil}
}

func returnAPIError(err error) *apiError {
	if err == nil {
		return nil
	}

	var eqc promql.ErrQueryCanceled
	var eqt promql.ErrQueryTimeout
	var es promql.ErrStorage
	switch {
	case errors.As(err, &eqc):
		return &apiError{errorCanceled, err}
	case errors.As(err, &eqt):
		return &apiError{errorTimeout, err}
	case errors.As(err, &es):
		return &apiError{errorInternal, err}
	}

	if errors.Is(err, context.Canceled) {
		return &apiError{errorCanceled, err}
	}

	return &apiError{errorExec, err}
}

func (api *API) labelNames(r *http.Request) apiFuncResult {
	limit, err := parseLimitParam(r.FormValue("limit"))
	if err != nil {
		return invalidParamError(err, "limit")
	}

	start, err := parseTimeParam(r, "start", MinTime)
	if err != nil {
		return invalidParamError(err, "start")
	}
	end, err := parseTimeParam(r, "end", MaxTime)
	if err != nil {
		return invalidParamError(err, "end")
	}

	matcherSets, err := parseMatchersParam(r.Form["match[]"])
	if err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, err}, nil, nil}
	}

	hints := &storage.LabelHints{
		Limit: toHintLimit(limit),
	}

	q, err := api.Queryable.Querier(timestamp.FromTime(start), timestamp.FromTime(end))
	if err != nil {
		return apiFuncResult{nil, returnAPIError(err), nil, nil}
	}
	defer q.Close()

	var (
		names    []string
		warnings annotations.Annotations
	)
	if len(matcherSets) > 1 {
		labelNamesSet := make(map[string]struct{})

		for _, matchers := range matcherSets {
			vals, callWarnings, err := q.LabelNames(r.Context(), hints, matchers...)
			if err != nil {
				return apiFuncResult{nil, returnAPIError(err), warnings, nil}
			}

			warnings.Merge(callWarnings)
			for _, val := range vals {
				labelNamesSet[val] = struct{}{}
			}
		}

		// Convert the map to an array.
		names = make([]string, 0, len(labelNamesSet))
		for key := range labelNamesSet {
			names = append(names, key)
		}
		slices.Sort(names)
	} else {
		var matchers []*labels.Matcher
		if len(matcherSets) == 1 {
			matchers = matcherSets[0]
		}
		names, warnings, err = q.LabelNames(r.Context(), hints, matchers...)
		if err != nil {
			return apiFuncResult{nil, &apiError{errorExec, err}, warnings, nil}
		}
	}

	if names == nil {
		names = []string{}
	}

	if limit > 0 && len(names) > limit {
		names = names[:limit]
		warnings = warnings.Add(errors.New("results truncated due to limit"))
	}
	return apiFuncResult{names, nil, warnings, nil}
}

func (api *API) labelValues(r *http.Request) (result apiFuncResult) {
	ctx := r.Context()
	name := route.Param(ctx, "name")

	if strings.HasPrefix(name, "U__") {
		name = model.UnescapeName(name, model.ValueEncodingEscaping)
	}

	label := model.LabelName(name)
	if !label.IsValid() {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("invalid label name: %q", name)}, nil, nil}
	}

	limit, err := parseLimitParam(r.FormValue("limit"))
	if err != nil {
		return invalidParamError(err, "limit")
	}

	start, err := parseTimeParam(r, "start", MinTime)
	if err != nil {
		return invalidParamError(err, "start")
	}
	end, err := parseTimeParam(r, "end", MaxTime)
	if err != nil {
		return invalidParamError(err, "end")
	}

	matcherSets, err := parseMatchersParam(r.Form["match[]"])
	if err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, err}, nil, nil}
	}

	hints := &storage.LabelHints{
		Limit: toHintLimit(limit),
	}

	q, err := api.Queryable.Querier(timestamp.FromTime(start), timestamp.FromTime(end))
	if err != nil {
		return apiFuncResult{nil, &apiError{errorExec, err}, nil, nil}
	}
	// From now on, we must only return with a finalizer in the result (to
	// be called by the caller) or call q.Close ourselves (which is required
	// in the case of a panic).
	defer func() {
		if result.finalizer == nil {
			q.Close()
		}
	}()
	closer := func() {
		q.Close()
	}

	var (
		vals     []string
		warnings annotations.Annotations
	)
	if len(matcherSets) > 1 {
		var callWarnings annotations.Annotations
		labelValuesSet := make(map[string]struct{})
		for _, matchers := range matcherSets {
			vals, callWarnings, err = q.LabelValues(ctx, name, hints, matchers...)
			if err != nil {
				return apiFuncResult{nil, &apiError{errorExec, err}, warnings, closer}
			}
			warnings.Merge(callWarnings)
			for _, val := range vals {
				labelValuesSet[val] = struct{}{}
			}
		}

		vals = make([]string, 0, len(labelValuesSet))
		for val := range labelValuesSet {
			vals = append(vals, val)
		}
	} else {
		var matchers []*labels.Matcher
		if len(matcherSets) == 1 {
			matchers = matcherSets[0]
		}
		vals, warnings, err = q.LabelValues(ctx, name, hints, matchers...)
		if err != nil {
			return apiFuncResult{nil, &apiError{errorExec, err}, warnings, closer}
		}

		if vals == nil {
			vals = []string{}
		}
	}

	slices.Sort(vals)

	if limit > 0 && len(vals) > limit {
		vals = vals[:limit]
		warnings = warnings.Add(errors.New("results truncated due to limit"))
	}

	return apiFuncResult{vals, nil, warnings, closer}
}

var (
	// MinTime is the default timestamp used for the start of optional time ranges.
	// Exposed to let downstream projects reference it.
	//
	// Historical note: This should just be time.Unix(math.MinInt64/1000, 0).UTC(),
	// but it was set to a higher value in the past due to a misunderstanding.
	// The value is still low enough for practical purposes, so we don't want
	// to change it now, avoiding confusion for importers of this variable.
	MinTime = time.Unix(math.MinInt64/1000+62135596801, 0).UTC()

	// MaxTime is the default timestamp used for the end of optional time ranges.
	// Exposed to let downstream projects to reference it.
	//
	// Historical note: This should just be time.Unix(math.MaxInt64/1000, 0).UTC(),
	// but it was set to a lower value in the past due to a misunderstanding.
	// The value is still high enough for practical purposes, so we don't want
	// to change it now, avoiding confusion for importers of this variable.
	MaxTime = time.Unix(math.MaxInt64/1000-62135596801, 999999999).UTC()

	minTimeFormatted = MinTime.Format(time.RFC3339Nano)
	maxTimeFormatted = MaxTime.Format(time.RFC3339Nano)
)

func (api *API) series(r *http.Request) (result apiFuncResult) {
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("error parsing form values: %w", err)}, nil, nil}
	}
	if len(r.Form["match[]"]) == 0 {
		return apiFuncResult{nil, &apiError{errorBadData, errors.New("no match[] parameter provided")}, nil, nil}
	}

	limit, err := parseLimitParam(r.FormValue("limit"))
	if err != nil {
		return invalidParamError(err, "limit")
	}

	start, err := parseTimeParam(r, "start", MinTime)
	if err != nil {
		return invalidParamError(err, "start")
	}
	end, err := parseTimeParam(r, "end", MaxTime)
	if err != nil {
		return invalidParamError(err, "end")
	}

	matcherSets, err := parseMatchersParam(r.Form["match[]"])
	if err != nil {
		return invalidParamError(err, "match[]")
	}

	q, err := api.Queryable.Querier(timestamp.FromTime(start), timestamp.FromTime(end))
	if err != nil {
		return apiFuncResult{nil, returnAPIError(err), nil, nil}
	}
	// From now on, we must only return with a finalizer in the result (to
	// be called by the caller) or call q.Close ourselves (which is required
	// in the case of a panic).
	defer func() {
		if result.finalizer == nil {
			q.Close()
		}
	}()
	closer := func() {
		q.Close()
	}

	hints := &storage.SelectHints{
		Start: timestamp.FromTime(start),
		End:   timestamp.FromTime(end),
		Func:  "series", // There is no series function, this token is used for lookups that don't need samples.
		Limit: toHintLimit(limit),
	}
	var set storage.SeriesSet

	if len(matcherSets) > 1 {
		var sets []storage.SeriesSet
		for _, mset := range matcherSets {
			// We need to sort this select results to merge (deduplicate) the series sets later.
			s := q.Select(ctx, true, hints, mset...)
			sets = append(sets, s)
		}
		set = storage.NewMergeSeriesSet(sets, 0, storage.ChainedSeriesMerge)
	} else {
		// At this point at least one match exists.
		set = q.Select(ctx, false, hints, matcherSets[0]...)
	}

	metrics := []labels.Labels{}

	warnings := set.Warnings()

	i := 1
	for set.Next() {
		if i%checkContextEveryNIterations == 0 {
			if err := ctx.Err(); err != nil {
				return apiFuncResult{nil, returnAPIError(err), warnings, closer}
			}
		}
		i++

		metrics = append(metrics, set.At().Labels())

		if limit > 0 && len(metrics) > limit {
			metrics = metrics[:limit]
			warnings.Add(errors.New("results truncated due to limit"))
			return apiFuncResult{metrics, nil, warnings, closer}
		}
	}
	if set.Err() != nil {
		return apiFuncResult{nil, returnAPIError(set.Err()), warnings, closer}
	}

	return apiFuncResult{metrics, nil, warnings, closer}
}

func (api *API) dropSeries(_ *http.Request) apiFuncResult {
	return apiFuncResult{nil, &apiError{errorInternal, errors.New("not implemented")}, nil, nil}
}

// Target has the information for one target.
type Target struct {
	// Labels before any processing.
	DiscoveredLabels labels.Labels `json:"discoveredLabels"`
	// Any labels that are added to this target and its metrics.
	Labels labels.Labels `json:"labels"`

	ScrapePool string `json:"scrapePool"`
	ScrapeURL  string `json:"scrapeUrl"`
	GlobalURL  string `json:"globalUrl"`

	LastError          string              `json:"lastError"`
	LastScrape         time.Time           `json:"lastScrape"`
	LastScrapeDuration float64             `json:"lastScrapeDuration"`
	Health             scrape.TargetHealth `json:"health"`

	ScrapeInterval string `json:"scrapeInterval"`
	ScrapeTimeout  string `json:"scrapeTimeout"`
}

type ScrapePoolsDiscovery struct {
	ScrapePools []string `json:"scrapePools"`
}

// DroppedTarget has the information for one target that was dropped during relabelling.
type DroppedTarget struct {
	// Labels before any processing.
	DiscoveredLabels labels.Labels `json:"discoveredLabels"`
	ScrapePool       string        `json:"scrapePool"`
}

// TargetDiscovery has all the active targets.
type TargetDiscovery struct {
	ActiveTargets       []*Target        `json:"activeTargets"`
	DroppedTargets      []*DroppedTarget `json:"droppedTargets"`
	DroppedTargetCounts map[string]int   `json:"droppedTargetCounts"`
}

// GlobalURLOptions contains fields used for deriving the global URL for local targets.
type GlobalURLOptions struct {
	ListenAddress string
	Host          string
	Scheme        string
}

// sanitizeSplitHostPort acts like net.SplitHostPort.
// Additionally, if there is no port in the host passed as input, we return the
// original host, making sure that IPv6 addresses are not surrounded by square
// brackets.
func sanitizeSplitHostPort(input string) (string, string, error) {
	host, port, err := net.SplitHostPort(input)
	if err != nil && strings.HasSuffix(err.Error(), "missing port in address") {
		var errWithPort error
		host, _, errWithPort = net.SplitHostPort(input + ":80")
		if errWithPort == nil {
			err = nil
		}
	}
	return host, port, err
}

func getGlobalURL(u *url.URL, opts GlobalURLOptions) (*url.URL, error) {
	host, port, err := sanitizeSplitHostPort(u.Host)
	if err != nil {
		return u, err
	}

	for _, lhr := range LocalhostRepresentations {
		if host == lhr {
			_, ownPort, err := net.SplitHostPort(opts.ListenAddress)
			if err != nil {
				return u, err
			}

			if port == ownPort {
				// Only in the case where the target is on localhost and its port is
				// the same as the one we're listening on, we know for sure that
				// we're monitoring our own process and that we need to change the
				// scheme, hostname, and port to the externally reachable ones as
				// well. We shouldn't need to touch the path at all, since if a
				// path prefix is defined, the path under which we scrape ourselves
				// should already contain the prefix.
				u.Scheme = opts.Scheme
				u.Host = opts.Host
			} else {
				// Otherwise, we only know that localhost is not reachable
				// externally, so we replace only the hostname by the one in the
				// external URL. It could be the wrong hostname for the service on
				// this port, but it's still the best possible guess.
				host, _, err := sanitizeSplitHostPort(opts.Host)
				if err != nil {
					return u, err
				}
				u.Host = host
				if port != "" {
					u.Host = net.JoinHostPort(u.Host, port)
				}
			}
			break
		}
	}

	return u, nil
}

func (api *API) scrapePools(r *http.Request) apiFuncResult {
	names := api.scrapePoolsRetriever(r.Context()).ScrapePools()
	sort.Strings(names)
	res := &ScrapePoolsDiscovery{ScrapePools: names}
	return apiFuncResult{data: res, err: nil, warnings: nil, finalizer: nil}
}

func (api *API) targets(r *http.Request) apiFuncResult {
	getSortedPools := func(targets map[string][]*scrape.Target) ([]string, int) {
		var n int
		pools := make([]string, 0, len(targets))
		for p, t := range targets {
			pools = append(pools, p)
			n += len(t)
		}
		slices.Sort(pools)
		return pools, n
	}

	scrapePool := r.URL.Query().Get("scrapePool")
	state := strings.ToLower(r.URL.Query().Get("state"))
	showActive := state == "" || state == "any" || state == "active"
	showDropped := state == "" || state == "any" || state == "dropped"
	res := &TargetDiscovery{}
	builder := labels.NewBuilder(labels.EmptyLabels())

	if showActive {
		targetsActive := api.targetRetriever(r.Context()).TargetsActive()
		activePools, numTargets := getSortedPools(targetsActive)
		res.ActiveTargets = make([]*Target, 0, numTargets)

		for _, pool := range activePools {
			if scrapePool != "" && pool != scrapePool {
				continue
			}
			for _, target := range targetsActive[pool] {
				lastErrStr := ""
				lastErr := target.LastError()
				if lastErr != nil {
					lastErrStr = lastErr.Error()
				}

				globalURL, err := getGlobalURL(target.URL(), api.globalURLOptions)

				res.ActiveTargets = append(res.ActiveTargets, &Target{
					DiscoveredLabels: target.DiscoveredLabels(builder),
					Labels:           target.Labels(builder),
					ScrapePool:       pool,
					ScrapeURL:        target.URL().String(),
					GlobalURL:        globalURL.String(),
					LastError: func() string {
						switch {
						case err == nil && lastErrStr == "":
							return ""
						case err != nil:
							return fmt.Errorf("%s: %w", lastErrStr, err).Error()
						default:
							return lastErrStr
						}
					}(),
					LastScrape:         target.LastScrape(),
					LastScrapeDuration: target.LastScrapeDuration().Seconds(),
					Health:             target.Health(),
					ScrapeInterval:     target.GetValue(model.ScrapeIntervalLabel),
					ScrapeTimeout:      target.GetValue(model.ScrapeTimeoutLabel),
				})
			}
		}
	} else {
		res.ActiveTargets = []*Target{}
	}
	if showDropped {
		res.DroppedTargetCounts = api.targetRetriever(r.Context()).TargetsDroppedCounts()

		targetsDropped := api.targetRetriever(r.Context()).TargetsDropped()
		droppedPools, numTargets := getSortedPools(targetsDropped)
		res.DroppedTargets = make([]*DroppedTarget, 0, numTargets)
		for _, pool := range droppedPools {
			if scrapePool != "" && pool != scrapePool {
				continue
			}
			for _, target := range targetsDropped[pool] {
				res.DroppedTargets = append(res.DroppedTargets, &DroppedTarget{
					DiscoveredLabels: target.DiscoveredLabels(builder),
					ScrapePool:       pool,
				})
			}
		}
	} else {
		res.DroppedTargets = []*DroppedTarget{}
	}
	return apiFuncResult{res, nil, nil, nil}
}

func matchLabels(lset labels.Labels, matchers []*labels.Matcher) bool {
	for _, m := range matchers {
		if !m.Matches(lset.Get(m.Name)) {
			return false
		}
	}
	return true
}

func (api *API) targetMetadata(r *http.Request) apiFuncResult {
	limit := -1
	if s := r.FormValue("limit"); s != "" {
		var err error
		if limit, err = strconv.Atoi(s); err != nil {
			return apiFuncResult{nil, &apiError{errorBadData, errors.New("limit must be a number")}, nil, nil}
		}
	}

	matchTarget := r.FormValue("match_target")
	var matchers []*labels.Matcher
	var err error
	if matchTarget != "" {
		matchers, err = parser.ParseMetricSelector(matchTarget)
		if err != nil {
			return invalidParamError(err, "match_target")
		}
	}

	builder := labels.NewBuilder(labels.EmptyLabels())
	metric := r.FormValue("metric")
	res := []metricMetadata{}
	for _, tt := range api.targetRetriever(r.Context()).TargetsActive() {
		for _, t := range tt {
			if limit >= 0 && len(res) >= limit {
				break
			}
			targetLabels := t.Labels(builder)
			// Filter targets that don't satisfy the label matchers.
			if matchTarget != "" && !matchLabels(targetLabels, matchers) {
				continue
			}
			// If no metric is specified, get the full list for the target.
			if metric == "" {
				for _, md := range t.ListMetadata() {
					res = append(res, metricMetadata{
						Target:       targetLabels,
						MetricFamily: md.MetricFamily,
						Type:         md.Type,
						Help:         md.Help,
						Unit:         md.Unit,
					})
				}
				continue
			}
			// Get metadata for the specified metric.
			if md, ok := t.GetMetadata(metric); ok {
				res = append(res, metricMetadata{
					Target: targetLabels,
					Type:   md.Type,
					Help:   md.Help,
					Unit:   md.Unit,
				})
			}
		}
	}

	return apiFuncResult{res, nil, nil, nil}
}

type metricMetadata struct {
	Target       labels.Labels    `json:"target"`
	MetricFamily string           `json:"metric,omitempty"`
	Type         model.MetricType `json:"type"`
	Help         string           `json:"help"`
	Unit         string           `json:"unit"`
}

// AlertmanagerDiscovery has all the active Alertmanagers.
type AlertmanagerDiscovery struct {
	ActiveAlertmanagers  []*AlertmanagerTarget `json:"activeAlertmanagers"`
	DroppedAlertmanagers []*AlertmanagerTarget `json:"droppedAlertmanagers"`
}

// AlertmanagerTarget has info on one AM.
type AlertmanagerTarget struct {
	URL string `json:"url"`
}

func (api *API) alertmanagers(r *http.Request) apiFuncResult {
	urls := api.alertmanagerRetriever(r.Context()).Alertmanagers()
	droppedURLS := api.alertmanagerRetriever(r.Context()).DroppedAlertmanagers()
	ams := &AlertmanagerDiscovery{ActiveAlertmanagers: make([]*AlertmanagerTarget, len(urls)), DroppedAlertmanagers: make([]*AlertmanagerTarget, len(droppedURLS))}
	for i, url := range urls {
		ams.ActiveAlertmanagers[i] = &AlertmanagerTarget{URL: url.String()}
	}
	for i, url := range droppedURLS {
		ams.DroppedAlertmanagers[i] = &AlertmanagerTarget{URL: url.String()}
	}
	return apiFuncResult{ams, nil, nil, nil}
}

// AlertDiscovery has info for all active alerts.
type AlertDiscovery struct {
	Alerts []*Alert `json:"alerts"`
}

// Alert has info for an alert.
type Alert struct {
	Labels          labels.Labels `json:"labels"`
	Annotations     labels.Labels `json:"annotations"`
	State           string        `json:"state"`
	ActiveAt        *time.Time    `json:"activeAt,omitempty"`
	KeepFiringSince *time.Time    `json:"keepFiringSince,omitempty"`
	Value           string        `json:"value"`
}

func (api *API) alerts(r *http.Request) apiFuncResult {
	alertingRules := api.rulesRetriever(r.Context()).AlertingRules()
	alerts := []*Alert{}

	for _, alertingRule := range alertingRules {
		alerts = append(
			alerts,
			rulesAlertsToAPIAlerts(alertingRule.ActiveAlerts())...,
		)
	}

	res := &AlertDiscovery{Alerts: alerts}

	return apiFuncResult{res, nil, nil, nil}
}

func rulesAlertsToAPIAlerts(rulesAlerts []*rules.Alert) []*Alert {
	apiAlerts := make([]*Alert, len(rulesAlerts))
	for i, ruleAlert := range rulesAlerts {
		apiAlerts[i] = &Alert{
			Labels:      ruleAlert.Labels,
			Annotations: ruleAlert.Annotations,
			State:       ruleAlert.State.String(),
			ActiveAt:    &ruleAlert.ActiveAt,
			Value:       strconv.FormatFloat(ruleAlert.Value, 'e', -1, 64),
		}
		if !ruleAlert.KeepFiringSince.IsZero() {
			apiAlerts[i].KeepFiringSince = &ruleAlert.KeepFiringSince
		}
	}

	return apiAlerts
}

func (api *API) metricMetadata(r *http.Request) apiFuncResult {
	metrics := map[string]map[metadata.Metadata]struct{}{}

	limit := -1
	if s := r.FormValue("limit"); s != "" {
		var err error
		if limit, err = strconv.Atoi(s); err != nil {
			return apiFuncResult{nil, &apiError{errorBadData, errors.New("limit must be a number")}, nil, nil}
		}
	}
	limitPerMetric := -1
	if s := r.FormValue("limit_per_metric"); s != "" {
		var err error
		if limitPerMetric, err = strconv.Atoi(s); err != nil {
			return apiFuncResult{nil, &apiError{errorBadData, errors.New("limit_per_metric must be a number")}, nil, nil}
		}
	}

	metric := r.FormValue("metric")
	for _, tt := range api.targetRetriever(r.Context()).TargetsActive() {
		for _, t := range tt {
			if metric == "" {
				for _, mm := range t.ListMetadata() {
					m := metadata.Metadata{Type: mm.Type, Help: mm.Help, Unit: mm.Unit}
					ms, ok := metrics[mm.MetricFamily]

					if limitPerMetric > 0 && len(ms) >= limitPerMetric {
						continue
					}

					if !ok {
						ms = map[metadata.Metadata]struct{}{}
						metrics[mm.MetricFamily] = ms
					}
					ms[m] = struct{}{}
				}
				continue
			}

			if md, ok := t.GetMetadata(metric); ok {
				m := metadata.Metadata{Type: md.Type, Help: md.Help, Unit: md.Unit}
				ms, ok := metrics[md.MetricFamily]

				if limitPerMetric > 0 && len(ms) >= limitPerMetric {
					continue
				}

				if !ok {
					ms = map[metadata.Metadata]struct{}{}
					metrics[md.MetricFamily] = ms
				}
				ms[m] = struct{}{}
			}
		}
	}

	// Put the elements from the pseudo-set into a slice for marshaling.
	res := map[string][]metadata.Metadata{}
	for name, set := range metrics {
		if limit >= 0 && len(res) >= limit {
			break
		}

		s := []metadata.Metadata{}
		for metadata := range set {
			s = append(s, metadata)
		}
		res[name] = s
	}

	return apiFuncResult{res, nil, nil, nil}
}

// RuleDiscovery has info for all rules.
type RuleDiscovery struct {
	RuleGroups     []*RuleGroup `json:"groups"`
	GroupNextToken string       `json:"groupNextToken,omitempty"`
}

// RuleGroup has info for rules which are part of a group.
type RuleGroup struct {
	Name string `json:"name"`
	File string `json:"file"`
	// In order to preserve rule ordering, while exposing type (alerting or recording)
	// specific properties, both alerting and recording rules are exposed in the
	// same array.
	Rules          []Rule    `json:"rules"`
	Interval       float64   `json:"interval"`
	Limit          int       `json:"limit"`
	EvaluationTime float64   `json:"evaluationTime"`
	LastEvaluation time.Time `json:"lastEvaluation"`
}

type Rule interface{}

type AlertingRule struct {
	// State can be "pending", "firing", "inactive".
	State          string           `json:"state"`
	Name           string           `json:"name"`
	Query          string           `json:"query"`
	Duration       float64          `json:"duration"`
	KeepFiringFor  float64          `json:"keepFiringFor"`
	Labels         labels.Labels    `json:"labels"`
	Annotations    labels.Labels    `json:"annotations"`
	Alerts         []*Alert         `json:"alerts"`
	Health         rules.RuleHealth `json:"health"`
	LastError      string           `json:"lastError,omitempty"`
	EvaluationTime float64          `json:"evaluationTime"`
	LastEvaluation time.Time        `json:"lastEvaluation"`
	// Type of an alertingRule is always "alerting".
	Type string `json:"type"`
}

type RecordingRule struct {
	Name           string           `json:"name"`
	Query          string           `json:"query"`
	Labels         labels.Labels    `json:"labels,omitempty"`
	Health         rules.RuleHealth `json:"health"`
	LastError      string           `json:"lastError,omitempty"`
	EvaluationTime float64          `json:"evaluationTime"`
	LastEvaluation time.Time        `json:"lastEvaluation"`
	// Type of a recordingRule is always "recording".
	Type string `json:"type"`
}

func (api *API) rules(r *http.Request) apiFuncResult {
	if err := r.ParseForm(); err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("error parsing form values: %w", err)}, nil, nil}
	}

	queryFormToSet := func(values []string) map[string]struct{} {
		set := make(map[string]struct{}, len(values))
		for _, v := range values {
			set[v] = struct{}{}
		}
		return set
	}

	rnSet := queryFormToSet(r.Form["rule_name[]"])
	rgSet := queryFormToSet(r.Form["rule_group[]"])
	fSet := queryFormToSet(r.Form["file[]"])

	matcherSets, err := parseMatchersParam(r.Form["match[]"])
	if err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, err}, nil, nil}
	}

	ruleGroups := api.rulesRetriever(r.Context()).RuleGroups()
	res := &RuleDiscovery{RuleGroups: make([]*RuleGroup, 0, len(ruleGroups))}
	typ := strings.ToLower(r.URL.Query().Get("type"))

	if typ != "" && typ != "alert" && typ != "record" {
		return invalidParamError(fmt.Errorf("not supported value %q", typ), "type")
	}

	returnAlerts := typ == "" || typ == "alert"
	returnRecording := typ == "" || typ == "record"

	excludeAlerts, err := parseExcludeAlerts(r)
	if err != nil {
		return invalidParamError(err, "exclude_alerts")
	}

	maxGroups, nextToken, parseErr := parseListRulesPaginationRequest(r)
	if parseErr != nil {
		return *parseErr
	}

	rgs := make([]*RuleGroup, 0, len(ruleGroups))

	foundToken := false

	for _, grp := range ruleGroups {
		if maxGroups > 0 && nextToken != "" && !foundToken {
			if nextToken != getRuleGroupNextToken(grp.File(), grp.Name()) {
				continue
			}
			foundToken = true
		}

		if len(rgSet) > 0 {
			if _, ok := rgSet[grp.Name()]; !ok {
				continue
			}
		}

		if len(fSet) > 0 {
			if _, ok := fSet[grp.File()]; !ok {
				continue
			}
		}

		apiRuleGroup := &RuleGroup{
			Name:           grp.Name(),
			File:           grp.File(),
			Interval:       grp.Interval().Seconds(),
			Limit:          grp.Limit(),
			Rules:          []Rule{},
			EvaluationTime: grp.GetEvaluationTime().Seconds(),
			LastEvaluation: grp.GetLastEvaluation(),
		}

		for _, rr := range grp.Rules(matcherSets...) {
			var enrichedRule Rule

			if len(rnSet) > 0 {
				if _, ok := rnSet[rr.Name()]; !ok {
					continue
				}
			}

			lastError := ""
			if rr.LastError() != nil {
				lastError = rr.LastError().Error()
			}
			switch rule := rr.(type) {
			case *rules.AlertingRule:
				if !returnAlerts {
					break
				}
				var activeAlerts []*Alert
				if !excludeAlerts {
					activeAlerts = rulesAlertsToAPIAlerts(rule.ActiveAlerts())
				}

				enrichedRule = AlertingRule{
					State:          rule.State().String(),
					Name:           rule.Name(),
					Query:          rule.Query().String(),
					Duration:       rule.HoldDuration().Seconds(),
					KeepFiringFor:  rule.KeepFiringFor().Seconds(),
					Labels:         rule.Labels(),
					Annotations:    rule.Annotations(),
					Alerts:         activeAlerts,
					Health:         rule.Health(),
					LastError:      lastError,
					EvaluationTime: rule.GetEvaluationDuration().Seconds(),
					LastEvaluation: rule.GetEvaluationTimestamp(),
					Type:           "alerting",
				}

			case *rules.RecordingRule:
				if !returnRecording {
					break
				}
				enrichedRule = RecordingRule{
					Name:           rule.Name(),
					Query:          rule.Query().String(),
					Labels:         rule.Labels(),
					Health:         rule.Health(),
					LastError:      lastError,
					EvaluationTime: rule.GetEvaluationDuration().Seconds(),
					LastEvaluation: rule.GetEvaluationTimestamp(),
					Type:           "recording",
				}
			default:
				err := fmt.Errorf("failed to assert type of rule '%v'", rule.Name())
				return apiFuncResult{nil, &apiError{errorInternal, err}, nil, nil}
			}

			if enrichedRule != nil {
				apiRuleGroup.Rules = append(apiRuleGroup.Rules, enrichedRule)
			}
		}

		// If the rule group response has no rules, skip it - this means we filtered all the rules of this group.
		if len(apiRuleGroup.Rules) > 0 {
			if maxGroups > 0 && len(rgs) == int(maxGroups) {
				// We've reached the capacity of our page plus one. That means that for sure there will be at least one
				// rule group in a subsequent request. Therefore a next token is required.
				res.GroupNextToken = getRuleGroupNextToken(grp.File(), grp.Name())
				break
			}
			rgs = append(rgs, apiRuleGroup)
		}
	}

	if maxGroups > 0 && nextToken != "" && !foundToken {
		return invalidParamError(fmt.Errorf("invalid group_next_token '%v'. were rule groups changed?", nextToken), "group_next_token")
	}

	res.RuleGroups = rgs
	return apiFuncResult{res, nil, nil, nil}
}

func parseExcludeAlerts(r *http.Request) (bool, error) {
	excludeAlertsParam := strings.ToLower(r.URL.Query().Get("exclude_alerts"))

	if excludeAlertsParam == "" {
		return false, nil
	}

	excludeAlerts, err := strconv.ParseBool(excludeAlertsParam)
	if err != nil {
		return false, fmt.Errorf("error converting exclude_alerts: %w", err)
	}
	return excludeAlerts, nil
}

func parseListRulesPaginationRequest(r *http.Request) (int64, string, *apiFuncResult) {
	var (
		parsedMaxGroups int64 = -1
		err             error
	)
	maxGroups := r.URL.Query().Get("group_limit")
	nextToken := r.URL.Query().Get("group_next_token")

	if nextToken != "" && maxGroups == "" {
		errResult := invalidParamError(errors.New("group_limit needs to be present in order to paginate over the groups"), "group_next_token")
		return -1, "", &errResult
	}

	if maxGroups != "" {
		parsedMaxGroups, err = strconv.ParseInt(maxGroups, 10, 32)
		if err != nil {
			errResult := invalidParamError(fmt.Errorf("group_limit needs to be a valid number: %w", err), "group_limit")
			return -1, "", &errResult
		}
		if parsedMaxGroups <= 0 {
			errResult := invalidParamError(errors.New("group_limit needs to be greater than 0"), "group_limit")
			return -1, "", &errResult
		}
	}

	if parsedMaxGroups > 0 {
		return parsedMaxGroups, nextToken, nil
	}

	return -1, "", nil
}

func getRuleGroupNextToken(file, group string) string {
	h := sha1.New()
	h.Write([]byte(file + ";" + group))
	return hex.EncodeToString(h.Sum(nil))
}

type prometheusConfig struct {
	YAML string `json:"yaml"`
}

func (api *API) serveRuntimeInfo(_ *http.Request) apiFuncResult {
	status, err := api.runtimeInfo()
	if err != nil {
		return apiFuncResult{status, &apiError{errorInternal, err}, nil, nil}
	}
	return apiFuncResult{status, nil, nil, nil}
}

func (api *API) serveBuildInfo(_ *http.Request) apiFuncResult {
	return apiFuncResult{api.buildInfo, nil, nil, nil}
}

func (api *API) serveConfig(_ *http.Request) apiFuncResult {
	cfg := &prometheusConfig{
		YAML: api.config().String(),
	}
	return apiFuncResult{cfg, nil, nil, nil}
}

func (api *API) serveFlags(_ *http.Request) apiFuncResult {
	return apiFuncResult{api.flagsMap, nil, nil, nil}
}

// TSDBStat holds the information about individual cardinality.
type TSDBStat struct {
	Name  string `json:"name"`
	Value uint64 `json:"value"`
}

// HeadStats has information about the TSDB head.
type HeadStats struct {
	NumSeries     uint64 `json:"numSeries"`
	NumLabelPairs int    `json:"numLabelPairs"`
	ChunkCount    int64  `json:"chunkCount"`
	MinTime       int64  `json:"minTime"`
	MaxTime       int64  `json:"maxTime"`
}

// TSDBStatus has information of cardinality statistics from postings.
type TSDBStatus struct {
	HeadStats                   HeadStats  `json:"headStats"`
	SeriesCountByMetricName     []TSDBStat `json:"seriesCountByMetricName"`
	LabelValueCountByLabelName  []TSDBStat `json:"labelValueCountByLabelName"`
	MemoryInBytesByLabelName    []TSDBStat `json:"memoryInBytesByLabelName"`
	SeriesCountByLabelValuePair []TSDBStat `json:"seriesCountByLabelValuePair"`
}

// TSDBStatsFromIndexStats converts a index.Stat slice to a TSDBStat slice.
func TSDBStatsFromIndexStats(stats []index.Stat) []TSDBStat {
	result := make([]TSDBStat, 0, len(stats))
	for _, item := range stats {
		item := TSDBStat{Name: item.Name, Value: item.Count}
		result = append(result, item)
	}
	return result
}

func (api *API) serveTSDBBlocks(_ *http.Request) apiFuncResult {
	blockMetas, err := api.db.BlockMetas()
	if err != nil {
		return apiFuncResult{nil, &apiError{errorInternal, fmt.Errorf("error getting block metadata: %w", err)}, nil, nil}
	}

	return apiFuncResult{
		data: map[string][]tsdb.BlockMeta{
			"blocks": blockMetas,
		},
	}
}

func (api *API) serveTSDBStatus(r *http.Request) apiFuncResult {
	limit := 10
	if s := r.FormValue("limit"); s != "" {
		var err error
		if limit, err = strconv.Atoi(s); err != nil || limit < 1 {
			return apiFuncResult{nil, &apiError{errorBadData, errors.New("limit must be a positive number")}, nil, nil}
		}
	}
	s, err := api.db.Stats(labels.MetricName, limit)
	if err != nil {
		return apiFuncResult{nil, &apiError{errorInternal, err}, nil, nil}
	}
	metrics, err := api.gatherer.Gather()
	if err != nil {
		return apiFuncResult{nil, &apiError{errorInternal, fmt.Errorf("error gathering runtime status: %w", err)}, nil, nil}
	}
	chunkCount := int64(math.NaN())
	for _, mF := range metrics {
		if *mF.Name == "prometheus_tsdb_head_chunks" {
			m := mF.Metric[0]
			if m.Gauge != nil {
				chunkCount = int64(m.Gauge.GetValue())
				break
			}
		}
	}
	return apiFuncResult{TSDBStatus{
		HeadStats: HeadStats{
			NumSeries:     s.NumSeries,
			ChunkCount:    chunkCount,
			MinTime:       s.MinTime,
			MaxTime:       s.MaxTime,
			NumLabelPairs: s.IndexPostingStats.NumLabelPairs,
		},
		SeriesCountByMetricName:     TSDBStatsFromIndexStats(s.IndexPostingStats.CardinalityMetricsStats),
		LabelValueCountByLabelName:  TSDBStatsFromIndexStats(s.IndexPostingStats.CardinalityLabelStats),
		MemoryInBytesByLabelName:    TSDBStatsFromIndexStats(s.IndexPostingStats.LabelValueStats),
		SeriesCountByLabelValuePair: TSDBStatsFromIndexStats(s.IndexPostingStats.LabelValuePairsStats),
	}, nil, nil, nil}
}

type walReplayStatus struct {
	Min     int `json:"min"`
	Max     int `json:"max"`
	Current int `json:"current"`
}

func (api *API) serveWALReplayStatus(w http.ResponseWriter, r *http.Request) {
	httputil.SetCORS(w, api.CORSOrigin, r)
	status, err := api.db.WALReplayStatus()
	if err != nil {
		api.respondError(w, &apiError{errorInternal, err}, nil)
	}
	api.respond(w, r, walReplayStatus{
		Min:     status.Min,
		Max:     status.Max,
		Current: status.Current,
	}, nil, "")
}

func (api *API) notifications(w http.ResponseWriter, r *http.Request) {
	httputil.SetCORS(w, api.CORSOrigin, r)
	api.respond(w, r, api.notificationsGetter(), nil, "")
}

func (api *API) notificationsSSE(w http.ResponseWriter, r *http.Request) {
	httputil.SetCORS(w, api.CORSOrigin, r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe to notifications.
	notifications, unsubscribe, ok := api.notificationsSub()
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer unsubscribe()

	// Set up a flusher to push the response to the client.
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Flush the response to ensure the headers are immediately and eventSource
	// onopen is triggered client-side.
	flusher.Flush()

	for {
		select {
		case notification := <-notifications:
			// Marshal the notification to JSON.
			jsonData, err := json.Marshal(notification)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				continue
			}

			// Write the event data in SSE format with JSON content.
			fmt.Fprintf(w, "data: %s\n\n", jsonData)

			// Flush the response to ensure the data is sent immediately.
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (api *API) remoteRead(w http.ResponseWriter, r *http.Request) {
	// This is only really for tests - this will never be nil IRL.
	if api.remoteReadHandler != nil {
		api.remoteReadHandler.ServeHTTP(w, r)
	} else {
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (api *API) remoteWrite(w http.ResponseWriter, r *http.Request) {
	if api.remoteWriteHandler != nil {
		api.remoteWriteHandler.ServeHTTP(w, r)
	} else {
		http.Error(w, "remote write receiver needs to be enabled with --web.enable-remote-write-receiver", http.StatusNotFound)
	}
}

func (api *API) otlpWrite(w http.ResponseWriter, r *http.Request) {
	if api.otlpWriteHandler != nil {
		api.otlpWriteHandler.ServeHTTP(w, r)
	} else {
		http.Error(w, "otlp write receiver needs to be enabled with --web.enable-otlp-receiver", http.StatusNotFound)
	}
}

func (api *API) deleteSeries(r *http.Request) apiFuncResult {
	if !api.enableAdmin {
		return apiFuncResult{nil, &apiError{errorUnavailable, errors.New("admin APIs disabled")}, nil, nil}
	}
	if err := r.ParseForm(); err != nil {
		return apiFuncResult{nil, &apiError{errorBadData, fmt.Errorf("error parsing form values: %w", err)}, nil, nil}
	}
	if len(r.Form["match[]"]) == 0 {
		return apiFuncResult{nil, &apiError{errorBadData, errors.New("no match[] parameter provided")}, nil, nil}
	}

	start, err := parseTimeParam(r, "start", MinTime)
	if err != nil {
		return invalidParamError(err, "start")
	}
	end, err := parseTimeParam(r, "end", MaxTime)
	if err != nil {
		return invalidParamError(err, "end")
	}

	for _, s := range r.Form["match[]"] {
		matchers, err := parser.ParseMetricSelector(s)
		if err != nil {
			return invalidParamError(err, "match[]")
		}
		if err := api.db.Delete(r.Context(), timestamp.FromTime(start), timestamp.FromTime(end), matchers...); err != nil {
			return apiFuncResult{nil, &apiError{errorInternal, err}, nil, nil}
		}
	}

	return apiFuncResult{nil, nil, nil, nil}
}

func (api *API) snapshot(r *http.Request) apiFuncResult {
	if !api.enableAdmin {
		return apiFuncResult{nil, &apiError{errorUnavailable, errors.New("admin APIs disabled")}, nil, nil}
	}
	var (
		skipHead bool
		err      error
	)
	if r.FormValue("skip_head") != "" {
		skipHead, err = strconv.ParseBool(r.FormValue("skip_head"))
		if err != nil {
			return invalidParamError(fmt.Errorf("unable to parse boolean: %w", err), "skip_head")
		}
	}

	var (
		snapdir = filepath.Join(api.dbDir, "snapshots")
		name    = fmt.Sprintf("%s-%016x",
			time.Now().UTC().Format("20060102T150405Z0700"),
			rand.Int63())
		dir = filepath.Join(snapdir, name)
	)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return apiFuncResult{nil, &apiError{errorInternal, fmt.Errorf("create snapshot directory: %w", err)}, nil, nil}
	}
	if err := api.db.Snapshot(dir, !skipHead); err != nil {
		return apiFuncResult{nil, &apiError{errorInternal, fmt.Errorf("create snapshot: %w", err)}, nil, nil}
	}

	return apiFuncResult{struct {
		Name string `json:"name"`
	}{name}, nil, nil, nil}
}

func (api *API) cleanTombstones(*http.Request) apiFuncResult {
	if !api.enableAdmin {
		return apiFuncResult{nil, &apiError{errorUnavailable, errors.New("admin APIs disabled")}, nil, nil}
	}
	if err := api.db.CleanTombstones(); err != nil {
		return apiFuncResult{nil, &apiError{errorInternal, err}, nil, nil}
	}

	return apiFuncResult{nil, nil, nil, nil}
}

// Query string is needed to get the position information for the annotations, and it
// can be empty if the position information isn't needed.
func (api *API) respond(w http.ResponseWriter, req *http.Request, data interface{}, warnings annotations.Annotations, query string) {
	statusMessage := statusSuccess
	warn, info := warnings.AsStrings(query, 10, 10)

	resp := &Response{
		Status:   statusMessage,
		Data:     data,
		Warnings: warn,
		Infos:    info,
	}

	codec, err := api.negotiateCodec(req, resp)
	if err != nil {
		api.respondError(w, &apiError{errorNotAcceptable, err}, nil)
		return
	}

	b, err := codec.Encode(resp)
	if err != nil {
		api.logger.Error("error marshaling response", "url", req.URL, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", codec.ContentType().String())
	w.WriteHeader(http.StatusOK)
	if n, err := w.Write(b); err != nil {
		api.logger.Error("error writing response", "url", req.URL, "bytesWritten", n, "err", err)
	}
}

func (api *API) negotiateCodec(req *http.Request, resp *Response) (Codec, error) {
	for _, clause := range goautoneg.ParseAccept(req.Header.Get("Accept")) {
		for _, codec := range api.codecs {
			if codec.ContentType().Satisfies(clause) && codec.CanEncode(resp) {
				return codec, nil
			}
		}
	}

	defaultCodec := api.codecs[0]
	if !defaultCodec.CanEncode(resp) {
		return nil, fmt.Errorf("cannot encode response as %s", defaultCodec.ContentType())
	}

	return defaultCodec, nil
}

func (api *API) respondError(w http.ResponseWriter, apiErr *apiError, data interface{}) {
	json := jsoniter.ConfigCompatibleWithStandardLibrary
	b, err := json.Marshal(&Response{
		Status:    statusError,
		ErrorType: apiErr.typ,
		Error:     apiErr.err.Error(),
		Data:      data,
	})
	if err != nil {
		api.logger.Error("error marshaling json response", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var code int
	switch apiErr.typ {
	case errorBadData:
		code = http.StatusBadRequest
	case errorExec:
		code = http.StatusUnprocessableEntity
	case errorCanceled:
		code = statusClientClosedConnection
	case errorTimeout:
		code = http.StatusServiceUnavailable
	case errorInternal:
		code = http.StatusInternalServerError
	case errorNotFound:
		code = http.StatusNotFound
	case errorNotAcceptable:
		code = http.StatusNotAcceptable
	default:
		code = http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if n, err := w.Write(b); err != nil {
		api.logger.Error("error writing response", "bytesWritten", n, "err", err)
	}
}

func parseTimeParam(r *http.Request, paramName string, defaultValue time.Time) (time.Time, error) {
	val := r.FormValue(paramName)
	if val == "" {
		return defaultValue, nil
	}
	result, err := parseTime(val)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time value for '%s': %w", paramName, err)
	}
	return result, nil
}

func parseTime(s string) (time.Time, error) {
	if t, err := strconv.ParseFloat(s, 64); err == nil {
		s, ns := math.Modf(t)
		ns = math.Round(ns*1000) / 1000
		return time.Unix(int64(s), int64(ns*float64(time.Second))).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}

	// Stdlib's time parser can only handle 4 digit years. As a workaround until
	// that is fixed we want to at least support our own boundary times.
	// Context: https://github.com/prometheus/client_golang/issues/614
	// Upstream issue: https://github.com/golang/go/issues/20555
	switch s {
	case minTimeFormatted:
		return MinTime, nil
	case maxTimeFormatted:
		return MaxTime, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse %q to a valid timestamp", s)
}

func parseDuration(s string) (time.Duration, error) {
	if d, err := strconv.ParseFloat(s, 64); err == nil {
		ts := d * float64(time.Second)
		if ts > float64(math.MaxInt64) || ts < float64(math.MinInt64) {
			return 0, fmt.Errorf("cannot parse %q to a valid duration. It overflows int64", s)
		}
		return time.Duration(ts), nil
	}
	if d, err := model.ParseDuration(s); err == nil {
		return time.Duration(d), nil
	}
	return 0, fmt.Errorf("cannot parse %q to a valid duration", s)
}

func parseMatchersParam(matchers []string) ([][]*labels.Matcher, error) {
	matcherSets, err := parser.ParseMetricSelectors(matchers)
	if err != nil {
		return nil, err
	}

OUTER:
	for _, ms := range matcherSets {
		for _, lm := range ms {
			if lm != nil && !lm.Matches("") {
				continue OUTER
			}
		}
		return nil, errors.New("match[] must contain at least one non-empty matcher")
	}
	return matcherSets, nil
}

// parseLimitParam returning 0 means no limit is to be applied.
func parseLimitParam(limitStr string) (limit int, err error) {
	if limitStr == "" {
		return limit, nil
	}

	limit, err = strconv.Atoi(limitStr)
	if err != nil {
		return limit, err
	}
	if limit < 0 {
		return limit, errors.New("limit must be non-negative")
	}

	return limit, nil
}

// toHintLimit increases the API limit, as returned by parseLimitParam, by 1.
// This allows for emitting warnings when the results are truncated.
func toHintLimit(limit int) int {
	// 0 means no limit and avoid int overflow
	if limit > 0 && limit < math.MaxInt {
		return limit + 1
	}
	return limit
}

// truncateResults truncates result for queryRange() and query().
// No truncation for other types(Scalars or Strings).
func truncateResults(result *promql.Result, limit int) (*promql.Result, bool) {
	isTruncated := false

	switch v := result.Value.(type) {
	case promql.Matrix:
		if len(v) > limit {
			result.Value = v[:limit]
			isTruncated = true
		}
	case promql.Vector:
		if len(v) > limit {
			result.Value = v[:limit]
			isTruncated = true
		}
	}

	// Return the modified result. Unchanged for other types.
	return result, isTruncated
}
