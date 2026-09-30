package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

type Server struct {
	cfg             Config
	client          *http.Client
	catalogClient   *http.Client
	catalog         *modelCatalog
	imageFiles      imageStore
	accounts        *accountPool
	sessions        adminSessions
	logs            *requestLogStore
	activeRequests  atomic.Int64
	startedAt       time.Time
	requestTimeout  time.Duration
	refreshInterval time.Duration
}

func New(c Config) (*Server, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 90 * time.Second
	transport.MaxIdleConnsPerHost = 32
	// Redirects must never move an authenticated request to another endpoint.
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	catalog, err := newCatalog()
	if err != nil {
		return nil, err
	}
	catalogClient := &http.Client{Timeout: catalogTimeout, CheckRedirect: client.CheckRedirect}
	accounts, err := newAccountPool(c)
	if err != nil {
		return nil, err
	}
	logs, err := newRequestLogStore(c.LogsDir)
	if err != nil {
		return nil, err
	}
	return &Server{logs: logs, cfg: c, client: client, catalogClient: catalogClient, catalog: catalog, accounts: accounts, startedAt: time.Now(), requestTimeout: 10 * time.Minute, refreshInterval: time.Hour}, nil
}

func (s *Server) ServeHTTP(out http.ResponseWriter, r *http.Request) {
	w := &responseWriter{ResponseWriter: out}
	defer func() { _ = http.NewResponseController(out).SetWriteDeadline(time.Time{}) }()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/files/file-") && strings.HasSuffix(r.URL.Path, "/content") {
		s.imageContent(w, r)
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/") {
		s.admin(w, r)
		return
	}
	if inferencePath(r) {
		var trace *requestTrace
		r, trace = s.beginTrace(w, r)
		defer func() {
			recovered := recover()
			s.endTrace(trace, r, recovered != nil)
			if recovered != nil {
				panic(recovered)
			}
		}()
	}
	protocol := "chat"
	if r.URL.Path == "/v1/messages" {
		protocol = "anthropic"
	}
	if r.URL.Path == "/v1/responses" {
		protocol = "responses"
	}
	gemini := r.URL.Path == "/v1beta/models" || strings.HasPrefix(r.URL.Path, "/v1beta/models/")
	if gemini {
		protocol = "gemini"
	}
	if !authorized(r, s.cfg.AuthToken, gemini) {
		writeError(w, protocol, 401, "invalid API key")
		return
	}
	if r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || strings.HasPrefix(r.URL.Path, "/v1/models/") || gemini) {
		s.models(w, r, protocol)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, protocol, 405, "method not allowed")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), accountBindingKey{}, &accountBinding{}))
	switch r.URL.Path {
	case "/v1/embeddings", "/v1/images/generations", "/v1/images/edits", "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/audio/translations", "/v1/rerank":
		s.mediaAPI(w, r)
		return
	}
	modelID := ""
	stream := false
	switch r.URL.Path {
	case "/v1/chat/completions", "/v1/messages", "/v1/responses":
	default:
		if !strings.HasPrefix(r.URL.Path, "/v1beta/models/") {
			writeError(w, protocol, 404, "endpoint not found")
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
		if strings.HasSuffix(path, ":streamGenerateContent") {
			stream = true
			modelID = strings.TrimSuffix(path, ":streamGenerateContent")
		} else if strings.HasSuffix(path, ":generateContent") {
			modelID = strings.TrimSuffix(path, ":generateContent")
		} else {
			writeError(w, protocol, 404, "endpoint not found")
			return
		}
		if modelID == "" {
			writeError(w, protocol, 400, "model is required")
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		status := 400
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = 413
		}
		writeError(w, protocol, status, "invalid or oversized request body")
		return
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil || raw == nil {
		writeError(w, protocol, 400, "request body must be a JSON object")
		return
	}
	if value, ok := raw["stream"]; ok {
		if _, ok := value.(bool); !ok {
			writeError(w, protocol, 400, "stream must be boolean")
			return
		}
	}
	switch protocol {
	case "responses":
		body, modelID, stream, err = convertResponsesToChat(body)
	case "anthropic":
		body, modelID, stream, err = convertAnthropicMessagesToChat(body)
	case "gemini":
		err = validateNativeGeminiRequest(raw)
		if err == nil {
			body, err = convertNativeGeminiToChat(modelID, body, stream)
		}
	default:
		modelID, err = requireString(raw, "model")
		stream = boolValue(raw["stream"])
		if err == nil {
			_, err = requireJSONArray(raw, "messages")
		}
	}
	if err != nil {
		writeError(w, protocol, 400, err.Error())
		return
	}
	// Preserve Chat Completions bytes and unknown provider options unless a model
	// alias is used. Adapted protocols use Workers AI's max_tokens parameter.
	target, err := s.resolveModel(modelID)
	if err != nil {
		writeError(w, protocol, 400, err.Error())
		return
	}
	traceModel(r.Context(), modelID, target, stream)
	if tasks := s.modelTasks(target); len(tasks) > 0 {
		chatCapable := taskChatModel(target)
		for _, task := range tasks {
			if task == "Text Generation" {
				chatCapable = true
			}
		}
		if !chatCapable {
			writeError(w, protocol, 400, "model is not a chat model; use the compatible endpoint listed in /v1/models")
			return
		}
	}
	if target != modelID || protocol != "chat" {
		// RawMessage preserves integers and provider-specific values exactly.
		var chat map[string]json.RawMessage
		if err = json.Unmarshal(body, &chat); err != nil {
			writeError(w, protocol, 400, err.Error())
			return
		}
		chat["model"], _ = json.Marshal(target)
		if protocol != "chat" {
			if limit, ok := chat["max_completion_tokens"]; ok {
				chat["max_tokens"] = limit
				delete(chat, "max_completion_tokens")
			}
			if stream {
				chat["stream_options"] = json.RawMessage(`{"include_usage":true}`)
			}
		}
		body, err = json.Marshal(chat)
		if err != nil {
			writeError(w, protocol, 400, err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()
	var resp *http.Response
	if taskChatModel(target) {
		resp, err = s.taskChat(ctx, w, target, modelID, body, stream)
	} else {
		accept := "application/json"
		if stream {
			accept = "text/event-stream"
		}
		resp, err = s.upstream(ctx, w, "/v1/chat/completions", "application/json", accept, body)
	}
	if err != nil {
		if r.Context().Err() == nil {
			status := transportStatus(err)
			message := "Cloudflare upstream connection failed or timed out"
			var failure *apiFailure
			if errors.As(err, &failure) {
				status = failure.status
				message = failure.message
			}
			writeError(w, protocol, status, message)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := readLimited(resp.Body, 1<<20)
		copyResponseHeaders(w.Header(), resp.Header, true)
		status := resp.StatusCode
		if status < 400 {
			status = 502
		}
		message := upstreamMessage(data, resp.StatusCode)
		message = redactAccountError(ctx, message)
		writeError(w, protocol, status, message)
		return
	}
	if stream {
		if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			writeError(w, protocol, 502, "upstream did not return an SSE stream")
			return
		}
		if protocol != "chat" {
			resp.Body = io.NopCloser(io.LimitReader(resp.Body, maxResponseBytes))
		}
		w.Header().Set("X-Accel-Buffering", "no")
		switch protocol {
		case "responses":
			err = streamChatAsResponses(w, resp, modelID)
		case "anthropic":
			err = streamChatAsAnthropic(w, resp, modelID)
		case "gemini":
			err = streamChatAsNativeGemini(w, resp, modelID)
		default:
			copyResponseHeaders(w.Header(), resp.Header, false)
			w.WriteHeader(resp.StatusCode)
			w.Flush()
			_, err = io.Copy(flushWriter{w}, resp.Body)
		}
		if err != nil && ctx.Err() == nil {
			writeError(w, protocol, 502, "upstream stream failed: "+err.Error())
		}
		return
	}
	data, err := readLimited(resp.Body, maxResponseBytes)
	if err != nil {
		writeError(w, protocol, 502, "cannot read upstream response")
		return
	}
	switch protocol {
	case "responses":
		data, err = convertChatJSONToResponses(data, modelID)
	case "anthropic":
		data, err = convertChatJSONToAnthropic(data, modelID)
	case "gemini":
		data, err = convertChatResponseToNativeGemini(data, modelID)
	}
	if err != nil {
		writeError(w, protocol, 502, "invalid upstream response: "+err.Error())
		return
	}
	copyResponseHeaders(w.Header(), resp.Header, protocol != "chat")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

func (s *Server) models(w *responseWriter, r *http.Request, protocol string) {
	names := s.modelRoutes()
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	prefix := "/v1/models"
	if protocol == "gemini" {
		prefix = "/v1beta/models"
	}
	requested := strings.TrimPrefix(r.URL.Path, prefix)
	if requested != "" {
		requested = strings.TrimPrefix(requested, "/")
		if _, ok := names[requested]; !ok {
			writeError(w, protocol, 404, "model not in configured catalog")
			return
		}
	}
	items := make([]any, 0, len(sorted))
	for _, name := range sorted {
		var item any = map[string]any{"id": name, "object": "model", "created": 0, "owned_by": "cloudflare", "tasks": s.modelTasks(names[name]), "supported_endpoints": s.modelEndpoints(names[name])}
		if protocol == "gemini" {
			tasks := s.modelTasks(names[name])
			methods := []string{}
			if len(tasks) == 0 {
				methods = []string{"generateContent", "streamGenerateContent"}
			}
			for _, task := range tasks {
				if task == "Text Generation" {
					methods = []string{"generateContent", "streamGenerateContent"}
					break
				}
			}
			item = map[string]any{"name": "models/" + name, "displayName": name, "supportedGenerationMethods": methods, "tasks": tasks}
		}
		if requested == name {
			writeJSON(w, 200, item)
			return
		}
		items = append(items, item)
	}
	if protocol == "gemini" {
		writeJSON(w, 200, map[string]any{"models": items})
	} else {
		writeJSON(w, 200, map[string]any{"object": "list", "data": items})
	}
}
