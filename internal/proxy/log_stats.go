package proxy

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

type logAggregate struct {
	Requests        int      `json:"requests"`
	Success         int      `json:"success"`
	Errors          int      `json:"errors"`
	Canceled        int      `json:"canceled"`
	Interrupted     int      `json:"interrupted"`
	SuccessRate     float64  `json:"success_rate"`
	InputTokens     int64    `json:"input_tokens"`
	OutputTokens    int64    `json:"output_tokens"`
	TotalTokens     int64    `json:"total_tokens"`
	CachedTokens    int64    `json:"cached_tokens"`
	ReasoningTokens int64    `json:"reasoning_tokens"`
	UsageReported   int      `json:"usage_reported_requests"`
	AvgDuration     float64  `json:"avg_duration_ms"`
	P95Duration     float64  `json:"p95_duration_ms"`
	AvgTTFB         *float64 `json:"avg_ttfb_ms"`
	RequestBytes    int64    `json:"request_bytes"`
	ResponseBytes   int64    `json:"response_bytes"`
	UpstreamCalls   int      `json:"upstream_calls"`
}
type groupStats struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
	logAggregate
}
type timeStats struct {
	Time time.Time `json:"time"`
	logAggregate
}

func aggregateLogs(records []requestSummary) logAggregate {
	a := logAggregate{Requests: len(records)}
	durations := make([]float64, 0, len(records))
	var ttfbTotal float64
	ttfbCount := 0
	for _, r := range records {
		switch r.Outcome {
		case "success":
			a.Success++
		case "canceled":
			a.Canceled++
		case "interrupted":
			a.Interrupted++
		default:
			a.Errors++
		}
		a.InputTokens += r.Usage.Input
		a.OutputTokens += r.Usage.Output
		a.TotalTokens += r.Usage.Total
		a.CachedTokens += r.Usage.Cached
		a.ReasoningTokens += r.Usage.Reasoning
		if r.Usage.Reported {
			a.UsageReported++
		}
		a.AvgDuration += r.DurationMS
		durations = append(durations, r.DurationMS)
		if r.TTFB != nil {
			ttfbTotal += *r.TTFB
			ttfbCount++
		}
		a.RequestBytes += r.RequestBytes
		a.ResponseBytes += r.ResponseBytes
		a.UpstreamCalls += r.UpstreamCalls
	}
	if a.Requests > 0 {
		a.SuccessRate = 100 * float64(a.Success) / float64(a.Requests)
		a.AvgDuration /= float64(a.Requests)
		sort.Float64s(durations)
		a.P95Duration = durations[int(math.Ceil(float64(len(durations))*.95))-1]
	}
	if ttfbCount > 0 {
		average := ttfbTotal / float64(ttfbCount)
		a.AvgTTFB = &average
	}
	return a
}
func groupLogs(records []requestSummary, byAccount bool) []groupStats {
	groups := map[string][]requestSummary{}
	names := map[string]string{}
	for _, r := range records {
		key := r.Model
		if byAccount {
			key = r.AccountID
			if _, ok := names[key]; !ok {
				names[key] = r.AccountName
			}
		}
		groups[key] = append(groups[key], r)
	}
	result := []groupStats{}
	for key, values := range groups {
		result = append(result, groupStats{Key: key, Name: names[key], logAggregate: aggregateLogs(values)})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Requests == result[j].Requests {
			return result[i].Key < result[j].Key
		}
		return result[i].Requests > result[j].Requests
	})
	return result
}
func timelineLogs(records []requestSummary, rangeName string) []timeStats {
	width, count := time.Hour, 24
	if rangeName != "24h" {
		width, count = 24*time.Hour, 7
	}
	start := time.Now().UTC().Add(-time.Duration(count) * width)
	groups := make([][]requestSummary, count)
	for _, r := range records {
		at := int(r.Started.Sub(start) / width)
		if r.Started.Before(start) {
			at = 0
		}
		if at >= 0 && at < count {
			groups[at] = append(groups[at], r)
		}
	}
	result := make([]timeStats, count)
	for i := range result {
		result[i] = timeStats{Time: start.Add(time.Duration(i) * width), logAggregate: aggregateLogs(groups[i])}
	}
	return result
}
func (s *Server) adminLogs(w *responseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "chat", 405, "method not allowed")
		return
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/admin/api/logs/") && path != "/admin/api/logs/export" {
		id := strings.TrimPrefix(path, "/admin/api/logs/")
		entry, err := s.logs.get(id)
		if err != nil {
			status := 500
			var failure *apiFailure
			if errors.As(err, &failure) {
				status = failure.status
			}
			writeError(w, "chat", status, err.Error())
			return
		}
		writeJSON(w, 200, entry)
		return
	}
	filter, err := parseLogFilter(r)
	if err != nil {
		writeError(w, "chat", 400, err.Error())
		return
	}
	records, storage := s.logs.matching(filter)
	switch path {
	case "/admin/api/logs":
		total := len(records)
		start := min((filter.page-1)*filter.size, total)
		end := min(start+filter.size, total)
		writeJSON(w, 200, map[string]any{"data": records[start:end], "total": total, "page": filter.page, "page_size": filter.size, "storage": storage, "active_requests": s.activeRequests.Load()})
	case "/admin/api/stats":
		writeJSON(w, 200, map[string]any{"summary": aggregateLogs(records), "by_model": groupLogs(records, false), "by_account": groupLogs(records, true), "timeline": timelineLogs(records, filter.rangeName), "active_requests": s.activeRequests.Load(), "storage": storage})
	case "/admin/api/logs/export":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="workersai-requests.ndjson"`)
		encoder := json.NewEncoder(w)
		for _, summary := range records {
			if r.Context().Err() != nil {
				return
			}
			entry, err := s.logs.get(summary.ID)
			if err != nil {
				continue
			}
			if encoder.Encode(entry) != nil {
				return
			}
		}
	default:
		writeError(w, "chat", 404, "endpoint not found")
	}
}
