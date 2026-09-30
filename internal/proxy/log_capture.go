package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const maxLogBody = 8 << 20

type tokenUsage struct {
	Reported  bool  `json:"reported"`
	Input     int64 `json:"input_tokens"`
	Output    int64 `json:"output_tokens"`
	Total     int64 `json:"total_tokens"`
	Cached    int64 `json:"cached_tokens"`
	Reasoning int64 `json:"reasoning_tokens"`
}

func (u *tokenUsage) add(v tokenUsage) {
	u.Reported = u.Reported || v.Reported
	u.Input += v.Input
	u.Output += v.Output
	u.Total += v.Total
	u.Cached += v.Cached
	u.Reasoning += v.Reasoning
}
func (u *tokenUsage) observe(value any) {
	obj, ok := value.(map[string]any)
	if !ok {
		return
	}
	if x, ok := obj["usage"].(map[string]any); ok {
		count := func(m map[string]any, keys ...string) int64 {
			for _, key := range keys {
				if n, ok := m[key].(float64); ok && n >= 0 && n < 1e15 {
					return int64(n)
				}
			}
			return 0
		}
		reported := false
		for _, key := range []string{"prompt_tokens", "input_tokens", "completion_tokens", "output_tokens", "total_tokens"} {
			if n, ok := x[key].(float64); ok && n >= 0 && n < 1e15 {
				reported = true
			}
		}
		u.Reported = u.Reported || reported
		u.Input = max(u.Input, count(x, "prompt_tokens", "input_tokens"))
		u.Output = max(u.Output, count(x, "completion_tokens", "output_tokens"))
		u.Total = max(u.Total, count(x, "total_tokens"))
		for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
			if detail, ok := x[key].(map[string]any); ok {
				u.Cached = max(u.Cached, count(detail, "cached_tokens"))
			}
		}
		for _, key := range []string{"completion_tokens_details", "output_tokens_details"} {
			if detail, ok := x[key].(map[string]any); ok {
				u.Reasoning = max(u.Reasoning, count(detail, "reasoning_tokens"))
			}
		}
		u.Total = max(u.Total, u.Input+u.Output)
	}
	for _, key := range []string{"result", "response", "message"} {
		if child, ok := obj[key].(map[string]any); ok {
			u.observe(child)
		}
	}
}

type bodyCapture struct {
	data         []byte
	size         int64
	contentType  string
	line         []byte
	droppingLine bool
	usage        tokenUsage
	done         bool
	readErr      error
	eof          bool
}

func (b *bodyCapture) Write(p []byte) (int, error) {
	originalSize := len(p)
	b.size += int64(len(p))
	remaining := maxLogBody - len(b.data)
	if remaining > 0 {
		b.data = append(b.data, p[:min(remaining, len(p))]...)
	}
	if strings.Contains(b.contentType, "text/event-stream") {
		for len(p) > 0 {
			at := bytes.IndexByte(p, '\n')
			part := p
			if at >= 0 {
				part = p[:at]
			}
			if !b.droppingLine {
				if len(b.line)+len(part) <= maxSSELineBytes {
					b.line = append(b.line, part...)
				} else {
					b.line = nil
					b.droppingLine = true
				}
			}
			if at < 0 {
				break
			}
			if !b.droppingLine {
				b.observeLine(b.line)
			}
			b.line = b.line[:0]
			b.droppingLine = false
			p = p[at+1:]
		}
	}
	return originalSize, nil
}
func (b *bodyCapture) observeLine(line []byte) {
	line = bytes.TrimSpace(line)
	if bytes.HasPrefix(line, []byte("data:")) {
		line = bytes.TrimSpace(line[5:])
	}
	if bytes.Equal(line, []byte("[DONE]")) {
		b.done = true
		return
	}
	var v any
	if json.Unmarshal(line, &v) == nil {
		b.usage.observe(v)
	}
}
func (b *bodyCapture) tokens() tokenUsage {
	if strings.Contains(b.contentType, "text/event-stream") {
		if len(b.line) > 0 {
			b.observeLine(b.line)
		}
	} else {
		var value any
		if json.Unmarshal(b.data, &value) == nil {
			b.usage.observe(value)
		}
	}
	return b.usage
}

type capturingBody struct {
	io.ReadCloser
	capture *bodyCapture
}

func (b *capturingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.capture.Write(p[:n])
	}
	if err == io.EOF {
		b.capture.eof = true
	} else if err != nil {
		b.capture.readErr = err
	}
	return n, err
}

type logPayload struct {
	ContentType string              `json:"content_type"`
	Headers     map[string][]string `json:"headers"`
	URL         string              `json:"url,omitempty"`
	Text        string              `json:"text"`
	Kind        string              `json:"kind"`
	Size        int64               `json:"size"`
	Truncated   bool                `json:"truncated"`
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	for _, suffix := range []string{"_token", "_secret", "_password", "_api_key", "_authorization"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	switch key {
	case "authorization", "proxy_authorization", "cookie", "set_cookie", "x_api_key", "x_goog_api_key", "api_key", "apikey", "api_token", "access_token", "refresh_token", "auth_token", "password", "secret", "client_secret", "token", "key":
		return true
	}
	return false
}
func maskText(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
			encoded, _ := json.Marshal(secret)
			escaped := string(encoded[1 : len(encoded)-1])
			if escaped != secret {
				text = strings.ReplaceAll(text, escaped, "[redacted]")
			}
		}
	}
	return text
}
func safeHeaders(headers http.Header, secrets []string) map[string][]string {
	out := map[string][]string{}
	for k, v := range headers {
		if sensitiveKey(k) {
			out[k] = []string{"[redacted]"}
			continue
		}
		for _, text := range v {
			out[k] = append(out[k], maskText(text, secrets))
		}
	}
	return out
}
func safeURL(raw string, secrets []string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	q := u.Query()
	for key, values := range q {
		if sensitiveKey(key) {
			q.Set(key, "[redacted]")
		} else {
			for i, v := range values {
				values[i] = maskText(v, secrets)
			}
			q[key] = values
		}
	}
	u.RawQuery = q.Encode()
	u.User = nil
	return maskText(u.String(), secrets)
}
func cleanValue(value any, secrets []string) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if sensitiveKey(key) {
				out[key] = "[redacted]"
				continue
			}
			switch key {
			case "b64_json", "image_b64":
				out[key] = "[base64 media omitted]"
				continue
			case "audio", "image", "mask":
				switch data := item.(type) {
				case string:
					out[key] = "[media payload omitted]"
					continue
				case []any:
					out[key] = map[string]any{"media_items": len(data)}
					continue
				}
			}
			out[key] = cleanValue(item, secrets)
		}
		return out
	case []string:
		out := make([]string, len(v))
		for i, item := range v {
			out[i] = maskText(item, secrets)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cleanValue(item, secrets)
		}
		return out
	case string:
		if strings.HasPrefix(v, "data:") && strings.Contains(v, ";base64,") {
			prefix, _, _ := strings.Cut(v, ",")
			return prefix + ",[media payload omitted]"
		}
		return maskText(v, secrets)
	default:
		return v
	}
}
func cleanJSON(data []byte, secrets []string) (string, bool) {
	var v any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&v) != nil {
		return "", false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return "", false
	}
	cleaned, err := json.MarshalIndent(cleanValue(v, secrets), "", "  ")
	return string(cleaned), err == nil
}
func payloadSnapshot(c *bodyCapture, headers http.Header, secrets []string) logPayload {
	p := logPayload{ContentType: c.contentType, Headers: safeHeaders(headers, secrets), Kind: "empty", Size: c.size, Truncated: c.size > int64(len(c.data))}
	if c.size == 0 {
		return p
	}
	typ, params, _ := mime.ParseMediaType(c.contentType)
	switch {
	case strings.HasPrefix(typ, "multipart/"):
		p.Kind = "multipart"
		fields := map[string]any{}
		files := []any{}
		reader := multipart.NewReader(bytes.NewReader(c.data), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err != nil {
				if err != io.EOF {
					p.Truncated = true
				}
				break
			}
			if part.FileName() != "" {
				size, err := io.Copy(io.Discard, part)
				files = append(files, map[string]any{"field": part.FormName(), "filename": part.FileName(), "content_type": part.Header.Get("Content-Type"), "size": size})
				if err != nil {
					p.Truncated = true
				}
			}
			if part.FileName() == "" {
				data, err := readLimited(part, 1<<20)
				if err != nil {
					p.Truncated = true
				} else {
					fields[part.FormName()] = string(data)
				}
			}
			part.Close()
		}
		encoded, _ := json.MarshalIndent(cleanValue(map[string]any{"fields": fields, "files": files}, secrets), "", "  ")
		p.Text = string(encoded)
	case strings.HasPrefix(typ, "audio/") || strings.HasPrefix(typ, "image/") || typ == "application/octet-stream":
		p.Kind = "binary"
		p.Truncated = false
		p.Text = "[binary media: contents omitted; size and content type recorded]"
	case typ == "text/event-stream":
		p.Kind = "sse"
		var out strings.Builder
		for _, line := range bytes.Split(c.data, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				body := bytes.TrimSpace(line[5:])
				if clean, ok := cleanJSON(body, secrets); ok {
					var compact bytes.Buffer
					json.Compact(&compact, []byte(clean))
					out.WriteString("data: ")
					out.Write(compact.Bytes())
				} else if bytes.HasPrefix(body, []byte("{")) || bytes.HasPrefix(body, []byte("[")) && !bytes.Equal(body, []byte("[DONE]")) {
					out.WriteString("data: [incomplete JSON event omitted]")
				} else {
					out.WriteString(maskText(string(line), secrets))
				}
			} else {
				out.WriteString(maskText(string(line), secrets))
			}
			out.WriteByte('\n')
		}
		p.Text = out.String()
	default:
		if text, ok := cleanJSON(c.data, secrets); ok {
			p.Kind = "json"
			p.Text = text
		} else if strings.Contains(typ, "json") || bytes.HasPrefix(bytes.TrimSpace(c.data), []byte("{")) || bytes.HasPrefix(bytes.TrimSpace(c.data), []byte("[")) {
			p.Kind = "text"
			p.Text = "[invalid or incomplete JSON body omitted; credentials and media cannot be safely separated]"
		} else if utf8.Valid(c.data) {
			p.Kind = "text"
			p.Text = maskText(string(c.data), secrets)
		} else {
			p.Kind = "binary"
			p.Text = "[binary contents omitted]"
		}
	}
	return p
}

type upstreamLog struct {
	Path       string     `json:"path"`
	Status     int        `json:"status_code"`
	DurationMS float64    `json:"duration_ms"`
	Error      string     `json:"error"`
	Request    logPayload `json:"request"`
	Response   logPayload `json:"response"`
}
type upstreamTrace struct {
	path            string
	started         time.Time
	finished        time.Time
	status          int
	errorText       string
	request         bodyCapture
	response        bodyCapture
	requestHeaders  http.Header
	responseHeaders http.Header
}
type upstreamCaptureBody struct {
	capturingBody
	trace *upstreamTrace
}

func (b *upstreamCaptureBody) Close() error {
	err := b.ReadCloser.Close()
	if b.trace.finished.IsZero() {
		b.trace.finished = time.Now()
	}
	return err
}
