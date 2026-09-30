package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxSSELineBytes = 8 << 20
const maxRequestBytes = 16 << 20
const maxResponseBytes = 32 << 20

// Only response metadata useful to API clients crosses the proxy boundary.
func copyResponseHeaders(dst, src http.Header, bodyModified bool) {
	blocked := map[string]bool{}
	for _, value := range src.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			blocked[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	for name, values := range src {
		key := strings.ToLower(name)
		if blocked[key] || key == "x-request-id" && dst.Get("X-Request-ID") != "" {
			continue
		}
		if key == "content-type" || key == "retry-after" || key == "cf-ray" || key == "x-request-id" || strings.HasPrefix(key, "x-ratelimit-") {
			dst[name] = append([]string(nil), values...)
		}
	}
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	return data, nil
}

// Each write gets an idle deadline, allowing long streams without letting a
// disconnected or slow consumer hold an upstream connection indefinitely.
type responseWriter struct {
	http.ResponseWriter
	committed bool
	trace     *requestTrace
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) WriteHeader(status int) {
	if !w.committed {
		if w.trace != nil {
			w.trace.writeStatus(status, w.Header())
		}
		w.committed = true
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *responseWriter) Write(p []byte) (int, error) {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(30 * time.Second))
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	if w.trace != nil {
		w.trace.writeBody(p[:n], err)
	}
	return n, err
}
func (w *responseWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(30 * time.Second))
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

type flushWriter struct{ w *responseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		f.w.Flush()
	}
	return n, err
}

func authorized(r *http.Request, token string, gemini bool) bool {
	candidate := ""
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		candidate = parts[1]
	}
	if candidate == "" {
		candidate = r.Header.Get("x-api-key")
	}
	if candidate == "" && gemini {
		candidate = r.Header.Get("x-goog-api-key")
		if candidate == "" {
			candidate = r.URL.Query().Get("key")
		}
	}
	a, b := sha256.Sum256([]byte(candidate)), sha256.Sum256([]byte(token))
	return candidate != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w *responseWriter, protocol string, status int, message string) {
	if w.trace != nil {
		w.trace.summary.Error = message
	}
	typ := "api_error"
	if status == 400 || status == 413 {
		typ = "invalid_request_error"
	}
	if status == 401 {
		typ = "authentication_error"
	}
	if status == 429 {
		typ = "rate_limit_error"
	}
	if status == 404 {
		typ = "not_found_error"
	}
	value := map[string]any{"error": map[string]any{"type": typ, "message": message}}
	if protocol == "anthropic" {
		value["type"] = "error"
	}
	if protocol == "gemini" {
		code := "INTERNAL"
		switch status {
		case 400, 413:
			code = "INVALID_ARGUMENT"
		case 401:
			code = "UNAUTHENTICATED"
		case 403:
			code = "PERMISSION_DENIED"
		case 404:
			code = "NOT_FOUND"
		case 429:
			code = "RESOURCE_EXHAUSTED"
		case 504:
			code = "DEADLINE_EXCEEDED"
		}
		value = map[string]any{"error": map[string]any{"code": status, "status": code, "message": message}}
	}
	if w.committed {
		payload, _ := json.Marshal(value)
		if protocol == "responses" {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
		} else if protocol == "anthropic" {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
		} else {
			fmt.Fprintf(w, "data: %s\n\n", payload)
		}
		w.Flush()
		return
	}
	writeJSON(w, status, value)
}

func upstreamMessage(body []byte, status int) string {
	var raw map[string]any
	if json.Unmarshal(body, &raw) == nil {
		if err, ok := raw["error"].(map[string]any); ok {
			if msg, ok := err["message"].(string); ok && msg != "" {
				return msg
			}
		}
		if errs, ok := raw["errors"].([]any); ok && len(errs) > 0 {
			if err, ok := errs[0].(map[string]any); ok {
				if msg, ok := err["message"].(string); ok && msg != "" {
					return msg
				}
			}
		}
	}
	return "Cloudflare upstream returned HTTP " + fmt.Sprint(status)
}

func transportStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}
