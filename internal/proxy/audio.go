package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (s *Server) speech(ctx context.Context, w *responseWriter, in mediaInput) error {
	raw := in.fields
	if err := mediaAllowed(raw, "model", "input", "voice", "response_format", "speed", "user", "lang"); err != nil {
		return err
	}
	_, id, err := s.mediaModel(raw, "aura-1", "Text-to-Speech")
	if err != nil {
		return err
	}
	text, err := requireString(raw, "input")
	if err != nil {
		return invalid("%s", err)
	}
	speed, err := mediaNumber(raw, "speed", 1)
	if err != nil {
		return err
	}
	if speed != 1 {
		return invalid("this speech model supports speed=1 only")
	}
	format, err := mediaString(raw, "response_format", "mp3")
	if err != nil {
		return err
	}
	mimeTypes := map[string]string{"mp3": "audio/mpeg", "wav": "audio/wav", "pcm": "audio/pcm", "flac": "audio/flac", "opus": "audio/ogg", "aac": "audio/aac"}
	mime, ok := mimeTypes[format]
	if !ok {
		return invalid("unsupported response_format")
	}
	voice, err := mediaString(raw, "voice", "alloy")
	if err != nil {
		return err
	}
	payload := map[string]any{}
	if shortName(id) == "melotts" {
		if format != "mp3" {
			return invalid("melotts supports response_format=mp3 only")
		}
		if voice != "alloy" && voice != "default" {
			return invalid("melotts does not support voice selection")
		}
		payload["prompt"] = text
		if lang := raw["lang"]; lang != nil {
			payload["lang"] = lang
		}
	} else {
		if raw["lang"] != nil {
			return invalid("select a language-specific aura model instead of lang")
		}
		payload["text"] = text
		voices := map[string]string{"alloy": "angus", "echo": "arcas", "fable": "orpheus", "onyx": "orion", "nova": "asteria", "shimmer": "luna"}
		if strings.HasPrefix(shortName(id), "aura-2-") {
			voices = map[string]string{"alloy": "thalia", "echo": "orion", "fable": "apollo", "onyx": "arcas", "nova": "asteria", "shimmer": "luna"}
			if shortName(id) == "aura-2-es" {
				voices = map[string]string{"alloy": "aquila", "echo": "nestor", "fable": "alvaro", "onyx": "sirio", "nova": "celeste", "shimmer": "selena"}
			}
		}
		if mapped, ok := voices[voice]; ok {
			voice = mapped
		}
		payload["speaker"] = voice
		switch format {
		case "wav":
			payload["encoding"] = "linear16"
			payload["container"] = "wav"
			payload["sample_rate"] = 24000
		case "pcm":
			payload["encoding"] = "linear16"
			payload["container"] = "none"
			payload["sample_rate"] = 24000
		case "opus":
			payload["encoding"] = "opus"
			payload["container"] = "ogg"
		default:
			payload["encoding"] = format
		}
	}
	resp, err := s.runModel(ctx, w, id, payload)
	if err != nil {
		return err
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		value, err := modelResult(resp)
		if err != nil {
			return err
		}
		obj, _ := value.(map[string]any)
		encoded, _ := obj["audio"].(string)
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) == 0 {
			return upstreamInvalid("upstream returned no valid audio")
		}
		w.Header().Set("Content-Type", mime)
		w.WriteHeader(200)
		_, err = w.Write(data)
		return err
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "audio/") && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/octet-stream") {
		return upstreamInvalid("upstream did not return audio")
	}
	w.Header().Set("Content-Type", mime)
	w.WriteHeader(200)
	if _, err := io.Copy(flushWriter{w}, resp.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

func (s *Server) transcribe(ctx context.Context, w *responseWriter, r *http.Request, in mediaInput) error {
	raw := in.fields
	if err := mediaAllowed(raw, "model", "language", "prompt", "response_format", "temperature"); err != nil {
		return err
	}
	for key := range in.files {
		if key != "file" {
			return invalid("unsupported file field %s", key)
		}
	}
	_, id, err := s.mediaModel(raw, "whisper-large-v3-turbo", "Automatic Speech Recognition")
	if err != nil {
		return err
	}
	name := shortName(id)
	if name == "flux" {
		return invalid("flux requires a realtime protocol; use whisper-large-v3-turbo or nova-3 for file transcription")
	}
	data, ct, err := readUpload(in.files, "file")
	if err != nil {
		return err
	}
	format, err := mediaString(raw, "response_format", "json")
	if err != nil {
		return err
	}
	switch format {
	case "json", "text", "verbose_json", "srt", "vtt":
	default:
		return invalid("unsupported response_format")
	}
	temperature, err := mediaNumber(raw, "temperature", 0)
	if err != nil {
		return err
	}
	if temperature != 0 {
		return invalid("temperature other than 0 is not supported")
	}
	translation := r.URL.Path == "/v1/audio/translations"
	if translation && name != "whisper-large-v3-turbo" {
		return invalid("audio translations require whisper-large-v3-turbo")
	}
	var resp *http.Response
	switch name {
	case "whisper-large-v3-turbo":
		payload := map[string]any{"audio": base64.StdEncoding.EncodeToString(data), "task": "transcribe"}
		if translation {
			payload["task"] = "translate"
		}
		for src, dst := range map[string]string{"language": "language", "prompt": "initial_prompt"} {
			if raw[src] != nil {
				payload[dst] = raw[src]
			}
		}
		resp, err = s.runModel(ctx, w, id, payload)
	case "whisper", "whisper-tiny-en":
		if raw["language"] != nil || raw["prompt"] != nil {
			return invalid("use whisper-large-v3-turbo for language and prompt options")
		}
		resp, err = s.runModel(ctx, w, id, map[string]any{"audio": byteNumbers(data)})
	case "nova-3":
		if raw["prompt"] != nil {
			return invalid("nova-3 does not support prompt")
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		path := "/run/" + id
		query := url.Values{}
		if lang, err := mediaString(raw, "language", ""); err != nil {
			return err
		} else if lang != "" {
			query.Set("language", lang)
		}
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
		resp, err = s.mediaUpstream(ctx, w, path, ct, data)
	default:
		return invalid("no compatible transcription adapter for model %s", name)
	}
	if err != nil {
		return err
	}
	value, err := modelResult(resp)
	if err != nil {
		return err
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return upstreamInvalid("invalid transcription response")
	}
	if name == "nova-3" {
		results, _ := obj["results"].(map[string]any)
		channels, _ := results["channels"].([]any)
		if len(channels) == 0 {
			return upstreamInvalid("no transcription channels")
		}
		channel, _ := channels[0].(map[string]any)
		alternatives, _ := channel["alternatives"].([]any)
		if len(alternatives) == 0 {
			return upstreamInvalid("no transcription alternatives")
		}
		alt, _ := alternatives[0].(map[string]any)
		obj = map[string]any{"text": alt["transcript"], "words": alt["words"], "language": channel["detected_language"]}
	}
	text, ok := obj["text"].(string)
	if !ok {
		return upstreamInvalid("upstream transcription has no text")
	}
	if format == "json" {
		writeJSON(w, 200, map[string]any{"text": text})
		return nil
	}
	if format == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, err = w.Write([]byte(text))
		return err
	}
	segments, _ := obj["segments"].([]any)
	if format == "srt" || format == "vtt" {
		if len(segments) == 0 && text != "" {
			return upstreamInvalid("model did not provide timestamps required for subtitles")
		}
		var out strings.Builder
		if format == "vtt" {
			out.WriteString("WEBVTT\n\n")
		}
		for i, v := range segments {
			segment, ok := v.(map[string]any)
			if !ok {
				return upstreamInvalid("invalid transcription segment")
			}
			start, ok1 := segment["start"].(float64)
			end, ok2 := segment["end"].(float64)
			if !ok1 || !ok2 {
				return upstreamInvalid("transcription segment has no timestamps")
			}
			fmt.Fprintf(&out, "%d\n%s --> %s\n%s\n\n", i+1, subtitleTime(start, format), subtitleTime(end, format), stringValue(segment["text"]))
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if format == "vtt" {
			w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		}
		_, err = w.Write([]byte(out.String()))
		return err
	}
	info, _ := obj["transcription_info"].(map[string]any)
	result := map[string]any{"task": "transcribe", "text": text}
	if translation {
		result["task"] = "translate"
	}
	for _, k := range []string{"language", "duration"} {
		if v := info[k]; v != nil {
			result[k] = v
		} else if v := obj[k]; v != nil {
			result[k] = v
		}
	}
	if segments != nil {
		for i, v := range segments {
			if segment, ok := v.(map[string]any); ok {
				if segment["id"] == nil {
					segment["id"] = i
				}
			}
		}
		result["segments"] = segments
	}
	if obj["words"] != nil {
		result["words"] = obj["words"]
	}
	writeJSON(w, 200, result)
	return nil
}
func subtitleTime(seconds float64, format string) string {
	ms := int64(seconds*1000 + 0.5)
	sep := ","
	if format == "vtt" {
		sep = "."
	}
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", ms/3600000, (ms/60000)%60, (ms/1000)%60, sep, ms%1000)
}
