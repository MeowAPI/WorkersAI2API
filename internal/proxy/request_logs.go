package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

type requestSummary struct {
	ID            string     `json:"id"`
	Started       time.Time  `json:"started_at"`
	Finished      time.Time  `json:"finished_at"`
	Method        string     `json:"method"`
	Path          string     `json:"path"`
	Model         string     `json:"model"`
	UpstreamModel string     `json:"upstream_model"`
	AccountID     string     `json:"account_id"`
	AccountName   string     `json:"account_name"`
	Status        int        `json:"status_code"`
	DurationMS    float64    `json:"duration_ms"`
	TTFB          *float64   `json:"ttfb_ms"`
	Stream        bool       `json:"stream"`
	Outcome       string     `json:"outcome"`
	Error         string     `json:"error"`
	RequestBytes  int64      `json:"request_bytes"`
	ResponseBytes int64      `json:"response_bytes"`
	Usage         tokenUsage `json:"usage"`
	UpstreamCalls int        `json:"upstream_calls"`
}
type requestLog struct {
	requestSummary
	Request  logPayload    `json:"request"`
	Response logPayload    `json:"response"`
	Upstream []upstreamLog `json:"upstream"`
}
type traceKey struct{}
type requestTrace struct {
	summary         requestSummary
	input           bodyCapture
	output          bodyCapture
	requestHeaders  http.Header
	responseHeaders http.Header
	requestURL      string
	steps           []*upstreamTrace
	secrets         []string
}

func traceFrom(ctx context.Context) *requestTrace {
	t, _ := ctx.Value(traceKey{}).(*requestTrace)
	return t
}
func inferencePath(r *http.Request) bool {
	if r.URL.Path == "/v1/models" || strings.HasPrefix(r.URL.Path, "/v1/models/") || strings.HasPrefix(r.URL.Path, "/v1/files/") {
		return false
	}
	if r.URL.Path == "/v1beta/models" || r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1beta/models/") {
		return false
	}
	return strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/v1beta/")
}
func (s *Server) beginTrace(w *responseWriter, r *http.Request) (*http.Request, *requestTrace) {
	trace := &requestTrace{summary: requestSummary{ID: randomID(), Started: time.Now(), Method: r.Method, Path: r.URL.Path}, requestHeaders: r.Header.Clone(), requestURL: r.URL.RequestURI(), secrets: []string{s.cfg.AuthToken}}
	trace.input.contentType = r.Header.Get("Content-Type")
	if r.Body != nil {
		r.Body = &capturingBody{ReadCloser: r.Body, capture: &trace.input}
	}
	w.trace = trace
	w.Header().Set("X-Request-ID", trace.summary.ID)
	s.activeRequests.Add(1)
	return r.WithContext(context.WithValue(r.Context(), traceKey{}, trace)), trace
}
func traceModel(ctx context.Context, model, upstream string, stream bool) {
	if trace := traceFrom(ctx); trace != nil {
		trace.summary.Model = model
		trace.summary.UpstreamModel = upstream
		trace.summary.Stream = stream
	}
}
func (trace *requestTrace) writeStatus(status int, headers http.Header) {
	trace.summary.Status = status
	trace.responseHeaders = headers.Clone()
	trace.output.contentType = headers.Get("Content-Type")
}
func (trace *requestTrace) writeBody(data []byte, err error) {
	if len(data) > 0 {
		if trace.summary.TTFB == nil {
			ms := float64(time.Since(trace.summary.Started).Microseconds()) / 1000
			trace.summary.TTFB = &ms
		}
		trace.output.Write(data)
	}
	if err != nil {
		trace.summary.Error = "client response write failed"
		trace.summary.Outcome = "interrupted"
	}
}
func (s *Server) endTrace(trace *requestTrace, r *http.Request, aborted bool) {
	defer s.activeRequests.Add(-1)
	trace.summary.Finished = time.Now()
	trace.summary.DurationMS = float64(trace.summary.Finished.Sub(trace.summary.Started).Microseconds()) / 1000
	trace.summary.RequestBytes = trace.input.size
	trace.summary.ResponseBytes = trace.output.size
	if trace.summary.Status == 0 {
		trace.summary.Status = 200
	}
	if aborted {
		trace.summary.Outcome = "interrupted"
		trace.summary.Error = "request handler interrupted"
		if trace.output.size == 0 {
			trace.summary.Status = 500
		}
	}
	if r.Context().Err() != nil {
		trace.summary.Outcome = "canceled"
		trace.summary.Error = "client disconnected or request canceled"
		if trace.output.size == 0 {
			trace.summary.Status = 499
		}
	}
	if trace.summary.Outcome == "" {
		if trace.summary.Status >= 400 || trace.summary.Error != "" {
			trace.summary.Outcome = "error"
		} else {
			trace.summary.Outcome = "success"
		}
	}
	s.accounts.mu.Lock()
	for _, a := range s.accounts.accounts {
		trace.secrets = append(trace.secrets, a.APIToken)
	}
	s.accounts.mu.Unlock()
	// Catch model fields even on local validation failures before routing.
	if trace.summary.Model == "" {
		var value map[string]any
		if json.Unmarshal(trace.input.data, &value) == nil {
			trace.summary.Model = stringValue(value["model"])
			trace.summary.Stream = boolValue(value["stream"])
		}
		if strings.HasPrefix(r.URL.Path, "/v1beta/models/") {
			trace.summary.Model = strings.Split(strings.TrimPrefix(r.URL.Path, "/v1beta/models/"), ":")[0]
		}
	}
	entry := requestLog{Upstream: []upstreamLog{}}
	for _, step := range trace.steps {
		trace.summary.Usage.add(step.response.tokens())
		if step.response.readErr != nil && step.errorText == "" {
			step.errorText = "upstream body read failed"
		}
		if step.status >= 200 && step.status < 300 && strings.Contains(step.response.contentType, "text/event-stream") && step.response.eof && !step.response.done && step.errorText == "" {
			step.errorText = "upstream stream ended without completion"
		}
		if step.errorText != "" && trace.summary.Outcome == "success" {
			trace.summary.Outcome = "interrupted"
			trace.summary.Error = step.errorText
		}
		finished := step.finished
		if finished.IsZero() {
			finished = trace.summary.Finished
		}
		entry.Upstream = append(entry.Upstream, upstreamLog{Path: safeURL(step.path, trace.secrets), Status: step.status, DurationMS: float64(finished.Sub(step.started).Microseconds()) / 1000, Error: maskText(step.errorText, trace.secrets), Request: payloadSnapshot(&step.request, step.requestHeaders, trace.secrets), Response: payloadSnapshot(&step.response, step.responseHeaders, trace.secrets)})
	}
	trace.summary.UpstreamCalls = len(trace.steps)
	trace.summary.Error = maskText(trace.summary.Error, trace.secrets)
	trace.summary.Model = maskText(trace.summary.Model, trace.secrets)
	trace.summary.UpstreamModel = maskText(trace.summary.UpstreamModel, trace.secrets)
	trace.summary.AccountName = maskText(trace.summary.AccountName, trace.secrets)
	trace.summary.Path = safeURL(trace.summary.Path, trace.secrets)
	entry.requestSummary = trace.summary
	entry.Request = payloadSnapshot(&trace.input, trace.requestHeaders, trace.secrets)
	if r.MultipartForm != nil {
		fields := map[string]any{}
		for key, values := range r.MultipartForm.Value {
			if len(values) == 1 {
				fields[key] = values[0]
			} else {
				fields[key] = values
			}
		}
		files := []any{}
		for field, parts := range r.MultipartForm.File {
			for _, part := range parts {
				files = append(files, map[string]any{"field": field, "filename": part.Filename, "size": part.Size, "content_type": part.Header.Get("Content-Type")})
			}
		}
		data, _ := json.MarshalIndent(cleanValue(map[string]any{"fields": fields, "files": files}, trace.secrets), "", "  ")
		entry.Request.Kind = "multipart"
		entry.Request.Text = string(data)
		entry.Request.Truncated = false
	}
	entry.Request.URL = safeURL(trace.requestURL, trace.secrets)
	entry.Response = payloadSnapshot(&trace.output, trace.responseHeaders, trace.secrets)
	if err := s.logs.add(entry); err != nil {
		log.Printf("Request log persistence failed for %s: %v", entry.ID, err)
	}
}
func (trace *requestTrace) beginUpstream(a account, path, contentType string, body []byte, headers http.Header) *upstreamTrace {
	trace.summary.AccountID = a.ID
	trace.summary.AccountName = a.Name
	trace.secrets = append(trace.secrets, a.APIToken)
	step := &upstreamTrace{path: path, started: time.Now(), requestHeaders: headers.Clone()}
	step.request.contentType = contentType
	step.request.Write(body)
	trace.steps = append(trace.steps, step)
	return step
}
func (step *upstreamTrace) gotResponse(resp *http.Response, err error) {
	if err != nil {
		step.errorText = "upstream connection failed"
		if errors.Is(err, context.DeadlineExceeded) {
			step.errorText = "upstream request timed out"
		}
		if errors.Is(err, context.Canceled) {
			step.errorText = "upstream request canceled"
		}
		step.finished = time.Now()
		return
	}
	step.status = resp.StatusCode
	step.responseHeaders = resp.Header.Clone()
	step.response.contentType = resp.Header.Get("Content-Type")
	resp.Body = &upstreamCaptureBody{capturingBody: capturingBody{ReadCloser: resp.Body, capture: &step.response}, trace: step}
}
