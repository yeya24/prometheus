// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	json "github.com/json-iterator/go"
	"github.com/prometheus/client_golang/api"
	"net/http"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
)

const maxSamplesInMemory = 5000

type queryRangeAPI interface {
	QueryRange(ctx context.Context, query string, r v1.Range) (model.Value, v1.Warnings, error)
}

// queryResult contains result data for a query.
type queryResult struct {
	Type   model.ValueType `json:"resultType"`
	Result interface{}     `json:"result"`

	// The decoded value.
	v model.Value
}

func (qr *queryResult) UnmarshalJSON(b []byte) error {
	v := struct {
		Type   model.ValueType `json:"resultType"`
		Result json.RawMessage `json:"result"`
	}{}

	err := json.Unmarshal(b, &v)
	if err != nil {
		return err
	}

	switch v.Type {
	case model.ValScalar:
		var sv model.Scalar
		err = json.Unmarshal(v.Result, &sv)
		qr.v = &sv

	case model.ValVector:
		var vv model.Vector
		err = json.Unmarshal(v.Result, &vv)
		qr.v = vv

	case model.ValMatrix:
		var mv model.Matrix
		err = json.Unmarshal(v.Result, &mv)
		qr.v = mv

	default:
		err = fmt.Errorf("unexpected value type %q", v.Type)
	}
	return err
}

type QueryRangeClient struct {
	client       api.Client
	downsampling time.Duration
}

func NewQueryRangeClient(client api.Client, downsampling time.Duration) *QueryRangeClient {
	return &QueryRangeClient{
		client:       client,
		downsampling: downsampling,
	}
}

func formatTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.Unix())+float64(t.Nanosecond())/1e9, 'f', -1, 64)
}

func (h *QueryRangeClient) QueryRange(ctx context.Context, query string, r v1.Range) (model.Value, v1.Warnings, error) {
	u := h.client.URL("/api/v1/query_range", nil)
	q := u.Query()

	q.Set("query", query)
	q.Set("start", formatTime(r.Start))
	q.Set("end", formatTime(r.End))
	q.Set("step", strconv.FormatFloat(r.Step.Seconds(), 'f', -1, 64))
	q.Set("max_source_resolution", "1h")
	u.RawQuery = q.Encode()
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, nil, err
	}

	_, body, warnings, err := h.Do(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	var qres queryResult

	return qres.v, warnings, json.Unmarshal(body, &qres)
}

// ErrorType models the different API error types.
type ErrorType string

type apiResponse struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType ErrorType       `json:"errorType"`
	Error     string          `json:"error"`
	Warnings  []string        `json:"warnings,omitempty"`
}

func apiError(code int) bool {
	// These are the codes that Prometheus sends when it returns an error.
	return code == http.StatusUnprocessableEntity || code == http.StatusBadRequest
}

// AlertState models the state of an alert.
type AlertState string

// HealthStatus models the health status of a scrape target.
type HealthStatus string

// RuleType models the type of a rule.
type RuleType string

// RuleHealth models the health status of a rule.
type RuleHealth string

const (
	// Possible values for AlertState.
	AlertStateFiring   AlertState = "firing"
	AlertStateInactive AlertState = "inactive"
	AlertStatePending  AlertState = "pending"

	// Possible values for ErrorType.
	ErrBadData     ErrorType = "bad_data"
	ErrTimeout     ErrorType = "timeout"
	ErrCanceled    ErrorType = "canceled"
	ErrExec        ErrorType = "execution"
	ErrBadResponse ErrorType = "bad_response"
	ErrServer      ErrorType = "server_error"
	ErrClient      ErrorType = "client_error"

	// Possible values for HealthStatus.
	HealthGood    HealthStatus = "up"
	HealthUnknown HealthStatus = "unknown"
	HealthBad     HealthStatus = "down"

	// Possible values for RuleType.
	RuleTypeRecording RuleType = "recording"
	RuleTypeAlerting  RuleType = "alerting"

	// Possible values for RuleHealth.
	RuleHealthGood    = "ok"
	RuleHealthUnknown = "unknown"
	RuleHealthBad     = "err"
)

func errorTypeAndMsgFor(resp *http.Response) (ErrorType, string) {
	switch resp.StatusCode / 100 {
	case 4:
		return ErrClient, fmt.Sprintf("client error: %d", resp.StatusCode)
	case 5:
		return ErrServer, fmt.Sprintf("server error: %d", resp.StatusCode)
	}
	return ErrBadResponse, fmt.Sprintf("bad response code %d", resp.StatusCode)
}

// Error is an error returned by the API.
type Error struct {
	Type   ErrorType
	Msg    string
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Msg)
}

func (h *QueryRangeClient) Do(ctx context.Context, req *http.Request) (*http.Response, []byte, v1.Warnings, error) {
	resp, body, err := h.client.Do(ctx, req)
	if err != nil {
		return resp, body, nil, err
	}

	code := resp.StatusCode

	if code/100 != 2 && !apiError(code) {
		errorType, errorMsg := errorTypeAndMsgFor(resp)
		return resp, body, nil, &Error{
			Type:   errorType,
			Msg:    errorMsg,
			Detail: string(body),
		}
	}

	var result apiResponse

	if http.StatusNoContent != code {
		if jsonErr := json.Unmarshal(body, &result); jsonErr != nil {
			return resp, body, nil, &Error{
				Type: ErrBadResponse,
				Msg:  jsonErr.Error(),
			}
		}
	}

	if apiError(code) && result.Status == "success" {
		err = &Error{
			Type: ErrBadResponse,
			Msg:  "inconsistent body for response code",
		}
	}

	if result.Status == "error" {
		err = &Error{
			Type: result.ErrorType,
			Msg:  result.Error,
		}
	}

	return resp, []byte(result.Data), result.Warnings, err
}

type ruleImporter struct {
	logger log.Logger
	config ruleImporterConfig

	apiClient queryRangeAPI

	groups      map[string]*rules.Group
	ruleManager *rules.Manager
}

type ruleImporterConfig struct {
	outputDir        string
	start            time.Time
	end              time.Time
	evalInterval     time.Duration
	maxBlockDuration time.Duration
}

// newRuleImporter creates a new rule importer that can be used to parse and evaluate recording rule files and create new series
// written to disk in blocks.
func newRuleImporter(logger log.Logger, config ruleImporterConfig, apiClient queryRangeAPI) *ruleImporter {
	level.Info(logger).Log("backfiller", "new rule importer", "start", config.start.Format(time.RFC822), "end", config.end.Format(time.RFC822))
	return &ruleImporter{
		logger:      logger,
		config:      config,
		apiClient:   apiClient,
		ruleManager: rules.NewManager(&rules.ManagerOptions{}),
	}
}

// loadGroups parses groups from a list of recording rule files.
func (importer *ruleImporter) loadGroups(ctx context.Context, filenames []string) (errs []error) {
	groups, errs := importer.ruleManager.LoadGroups(importer.config.evalInterval, labels.Labels{}, "", nil, filenames...)
	if errs != nil {
		return errs
	}
	importer.groups = groups
	return nil
}

// importAll evaluates all the recording rules and creates new time series and writes them to disk in blocks.
func (importer *ruleImporter) importAll(ctx context.Context) (errs []error) {
	for name, group := range importer.groups {
		level.Info(importer.logger).Log("backfiller", "processing group", "name", name)

		for i, r := range group.Rules() {
			level.Info(importer.logger).Log("backfiller", "processing rule", "id", i, "name", r.Name())
			if err := importer.importRule(ctx, r.Query().String(), r.Name(), r.Labels(), importer.config.start, importer.config.end, int64(importer.config.maxBlockDuration/time.Millisecond), group); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// importRule queries a prometheus API to evaluate rules at times in the past.
func (importer *ruleImporter) importRule(ctx context.Context, ruleExpr, ruleName string, ruleLabels labels.Labels, start, end time.Time,
	maxBlockDuration int64, grp *rules.Group,
) (err error) {
	blockDuration := getCompatibleBlockDuration(maxBlockDuration)
	startInMs := start.Unix() * int64(time.Second/time.Millisecond)
	endInMs := end.Unix() * int64(time.Second/time.Millisecond)

	for startOfBlock := blockDuration * (startInMs / blockDuration); startOfBlock <= endInMs; startOfBlock = startOfBlock + blockDuration {
		endOfBlock := startOfBlock + blockDuration - 1

		currStart := max(startOfBlock/int64(time.Second/time.Millisecond), start.Unix())
		startWithAlignment := grp.EvalTimestamp(time.Unix(currStart, 0).UTC().UnixNano())
		for startWithAlignment.Unix() < currStart {
			startWithAlignment = startWithAlignment.Add(grp.Interval())
		}
		end := time.Unix(min(endOfBlock/int64(time.Second/time.Millisecond), end.Unix()), 0).UTC()
		if end.Before(startWithAlignment) {
			break
		}
		val, warnings, err := importer.apiClient.QueryRange(ctx,
			ruleExpr,
			v1.Range{
				Start: startWithAlignment,
				End:   end,
				Step:  grp.Interval(),
			},
		)
		if err != nil {
			return fmt.Errorf("query range: %w", err)
		}
		if warnings != nil {
			level.Warn(importer.logger).Log("msg", "Range query returned warnings.", "warnings", warnings)
		}

		// To prevent races with compaction, a block writer only allows appending samples
		// that are at most half a block size older than the most recent sample appended so far.
		// However, in the way we use the block writer here, compaction doesn't happen, while we
		// also need to append samples throughout the whole block range. To allow that, we
		// pretend that the block is twice as large here, but only really add sample in the
		// original interval later.
		w, err := tsdb.NewBlockWriter(log.NewNopLogger(), importer.config.outputDir, 2*blockDuration)
		if err != nil {
			return fmt.Errorf("new block writer: %w", err)
		}
		var closed bool
		defer func() {
			if !closed {
				err = tsdb_errors.NewMulti(err, w.Close()).Err()
			}
		}()
		app := newMultipleAppender(ctx, w)
		var matrix model.Matrix
		switch val.Type() {
		case model.ValMatrix:
			matrix = val.(model.Matrix)

			for _, sample := range matrix {
				lb := labels.NewBuilder(labels.Labels{})

				for name, value := range sample.Metric {
					lb.Set(string(name), string(value))
				}

				// Setting the rule labels after the output of the query,
				// so they can override query output.
				for _, l := range ruleLabels {
					lb.Set(l.Name, l.Value)
				}

				lb.Set(labels.MetricName, ruleName)

				for _, value := range sample.Values {
					if err := app.add(ctx, lb.Labels(), timestamp.FromTime(value.Timestamp.Time()), float64(value.Value)); err != nil {
						return fmt.Errorf("add: %w", err)
					}
				}
			}
		default:
			return fmt.Errorf("rule result is wrong type %s", val.Type().String())
		}

		if err := app.flushAndCommit(ctx); err != nil {
			return fmt.Errorf("flush and commit: %w", err)
		}
		err = tsdb_errors.NewMulti(err, w.Close()).Err()
		closed = true
	}

	return err
}

func newMultipleAppender(ctx context.Context, blockWriter *tsdb.BlockWriter) *multipleAppender {
	return &multipleAppender{
		maxSamplesInMemory: maxSamplesInMemory,
		writer:             blockWriter,
		appender:           blockWriter.Appender(ctx),
	}
}

// multipleAppender keeps track of how many series have been added to the current appender.
// If the max samples have been added, then all series are committed and a new appender is created.
type multipleAppender struct {
	maxSamplesInMemory int
	currentSampleCount int
	writer             *tsdb.BlockWriter
	appender           storage.Appender
}

func (m *multipleAppender) add(ctx context.Context, l labels.Labels, t int64, v float64) error {
	if _, err := m.appender.Append(0, l, t, v); err != nil {
		return fmt.Errorf("multiappender append: %w", err)
	}
	m.currentSampleCount++
	if m.currentSampleCount >= m.maxSamplesInMemory {
		return m.commit(ctx)
	}
	return nil
}

func (m *multipleAppender) commit(ctx context.Context) error {
	if m.currentSampleCount == 0 {
		return nil
	}
	if err := m.appender.Commit(); err != nil {
		return fmt.Errorf("multiappender commit: %w", err)
	}
	m.appender = m.writer.Appender(ctx)
	m.currentSampleCount = 0
	return nil
}

func (m *multipleAppender) flushAndCommit(ctx context.Context) error {
	if err := m.commit(ctx); err != nil {
		return err
	}
	if _, err := m.writer.Flush(ctx); err != nil {
		return fmt.Errorf("multiappender flush: %w", err)
	}
	return nil
}

func max(x, y int64) int64 {
	if x > y {
		return x
	}
	return y
}

func min(x, y int64) int64 {
	if x < y {
		return x
	}
	return y
}
