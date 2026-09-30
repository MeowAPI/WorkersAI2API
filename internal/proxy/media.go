package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
)

const maxMediaRequestBytes = 64 << 20
const maxMediaResponseBytes = 64 << 20

type apiFailure struct {
	status  int
	message string
}

func (e *apiFailure) Error() string            { return e.message }
func invalid(format string, args ...any) error { return &apiFailure{400, fmt.Sprintf(format, args...)} }
func upstreamInvalid(format string, args ...any) error {
	return &apiFailure{502, fmt.Sprintf(format, args...)}
}

type mediaInput struct {
	fields map[string]any
	files  map[string][]*multipart.FileHeader
}

func readMediaInput(w http.ResponseWriter, r *http.Request) (mediaInput, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMediaRequestBytes)
	in := mediaInput{fields: map[string]any{}, files: map[string][]*multipart.FileHeader{}}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			return in, err
		}
		for k, v := range r.MultipartForm.Value {
			if len(v) != 1 {
				return in, invalid("multiple values for %s are not supported", k)
			}
			in.fields[k] = v[0]
		}
		in.files = r.MultipartForm.File
	} else {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return in, err
		}
		if json.Unmarshal(data, &in.fields) != nil || in.fields == nil {
			return in, invalid("request body must be a JSON object")
		}
	}
	return in, nil
}
func mediaNumber(raw map[string]any, key string, fallback float64) (float64, error) {
	v, ok := raw[key]
	if !ok {
		return fallback, nil
	}
	if n, ok := v.(float64); ok && !math.IsNaN(n) && !math.IsInf(n, 0) {
		return n, nil
	}
	if s, ok := v.(string); ok {
		if n, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
			return n, nil
		}
	}
	return 0, invalid("%s must be a number", key)
}
func mediaString(raw map[string]any, key, fallback string) (string, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return fallback, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", invalid("%s must be a string", key)
	}
	return s, nil
}
func mediaAllowed(raw map[string]any, keys ...string) error {
	if err := rejectUnsupportedFields(raw, "API", keys...); err != nil {
		return invalid("%s", err)
	}
	return nil
}
func readUpload(files map[string][]*multipart.FileHeader, key string) ([]byte, string, error) {
	parts := files[key]
	if len(parts) == 0 {
		return nil, "", invalid("%s file is required", key)
	}
	if len(parts) != 1 {
		return nil, "", invalid("exactly one %s file is supported", key)
	}
	f, err := parts[0].Open()
	if err != nil {
		return nil, "", invalid("cannot open %s", key)
	}
	defer f.Close()
	data, err := readLimited(f, maxMediaRequestBytes)
	if err != nil {
		return nil, "", invalid("cannot read %s", key)
	}
	if len(data) == 0 {
		return nil, "", invalid("%s is empty", key)
	}
	return data, parts[0].Header.Get("Content-Type"), nil
}
func (s *Server) mediaModel(raw map[string]any, fallback, task string) (string, string, error) {
	name, err := mediaString(raw, "model", fallback)
	if err != nil {
		return "", "", err
	}
	if name == "" {
		return "", "", invalid("model is required")
	}
	id, err := s.resolveModel(name)
	if err != nil {
		return "", "", invalid("%s", err)
	}
	if tasks := s.modelTasks(id); len(tasks) > 0 && task != "" {
		found := false
		for _, t := range tasks {
			found = found || t == task
		}
		if !found {
			return "", "", invalid("model %s does not support %s", name, task)
		}
	}
	return name, id, nil
}

func (s *Server) mediaAPI(w *responseWriter, r *http.Request) {
	in, err := readMediaInput(w, r)
	defer func() {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
	}()
	if err != nil {
		var large *http.MaxBytesError
		status := 400
		if errors.As(err, &large) {
			status = 413
		}
		writeError(w, "chat", status, err.Error())
		return
	}
	if model, ok := in.fields["model"].(string); ok {
		target, _ := s.resolveModel(model)
		traceModel(r.Context(), model, target, false)
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()
	switch r.URL.Path {
	case "/v1/embeddings":
		err = s.embeddings(ctx, w, in)
	case "/v1/images/generations", "/v1/images/edits":
		err = s.images(ctx, w, r, in)
	case "/v1/audio/speech":
		err = s.speech(ctx, w, in)
	case "/v1/audio/transcriptions", "/v1/audio/translations":
		err = s.transcribe(ctx, w, r, in)
	case "/v1/rerank":
		err = s.rerank(ctx, w, in)
	}
	if err != nil && r.Context().Err() == nil {
		status := 502
		var failure *apiFailure
		if errors.As(err, &failure) {
			status = failure.status
		} else if ctx.Err() != nil {
			status = 504
		}
		writeError(w, "chat", status, err.Error())
	}
}

func (s *Server) mediaUpstream(ctx context.Context, w http.ResponseWriter, path, contentType string, body []byte) (*http.Response, error) {
	resp, err := s.upstream(ctx, w, path, contentType, "", body)
	if err != nil {
		var failure *apiFailure
		if errors.As(err, &failure) {
			return nil, err
		}
		return nil, &apiFailure{transportStatus(err), "Cloudflare upstream connection failed or timed out"}
	}
	copyResponseHeaders(w.Header(), resp.Header, true)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := readLimited(resp.Body, 1<<20)
		status := resp.StatusCode
		if status < 400 {
			status = 502
		}
		return nil, &apiFailure{status, redactAccountError(ctx, upstreamMessage(data, resp.StatusCode))}
	}
	return resp, nil
}
func (s *Server) runModel(ctx context.Context, w http.ResponseWriter, id string, body any) (*http.Response, error) {
	if !fullModelID.MatchString(id) {
		return nil, invalid("invalid upstream model ID")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, invalid("invalid model input")
	}
	return s.mediaUpstream(ctx, w, "/run/"+id, "application/json", data)
}
func modelResult(resp *http.Response) (any, error) {
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, maxMediaResponseBytes)
	if err != nil {
		return nil, upstreamInvalid("upstream response too large or truncated")
	}
	var value any
	if json.Unmarshal(body, &value) != nil {
		return nil, upstreamInvalid("upstream returned invalid JSON")
	}
	if obj, ok := value.(map[string]any); ok {
		if success, ok := obj["success"].(bool); ok && !success {
			return nil, upstreamInvalid("upstream model failed")
		}
		if result, ok := obj["result"]; ok {
			return result, nil
		}
	}
	return value, nil
}
func (s *Server) embeddings(ctx context.Context, w *responseWriter, in mediaInput) error {
	_, id, err := s.mediaModel(in.fields, "", "Text Embeddings")
	if err != nil {
		return err
	}
	if in.fields["input"] == nil {
		return invalid("input is required")
	}
	in.fields["model"] = id
	body, err := json.Marshal(in.fields)
	if err != nil {
		return invalid("invalid embedding input")
	}
	resp, err := s.mediaUpstream(ctx, w, "/v1/embeddings", "application/json", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := readLimited(resp.Body, maxMediaResponseBytes)
	if err != nil {
		return upstreamInvalid("cannot read embedding response")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, err = w.Write(data)
	return err
}
