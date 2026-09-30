package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAIEmbeddingsRoute(t *testing.T) {
	result := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`
	s := testServerForPath(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		raw := object(t, body)
		if raw["model"] != "@cf/test/model" || raw["input"].([]any)[0] != "hello" || raw["encoding_format"] != "float" {
			t.Fatal(raw)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, result)
	}, "/v1/embeddings")
	w := request(s, "/v1/embeddings", `{"model":"model","input":["hello"],"encoding_format":"float"}`)
	if w.Code != 200 || w.Body.String() != result {
		t.Fatal(w.Code, w.Body)
	}
}

func TestEmbeddingsErrorsUseOpenAIShape(t *testing.T) {
	s := testServerForPath(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"errors":[{"message":"invalid embedding input"}]}`)
	}, "/v1/embeddings")
	w := request(s, "/v1/embeddings", `{"model":"model","input":"hello"}`)
	if w.Code != 400 || object(t, w.Body.Bytes())["error"].(map[string]any)["message"] != "invalid embedding input" {
		t.Fatal(w.Code, w.Body)
	}
}

func TestImagesRejectNonFiniteNumbers(t *testing.T) {
	s := testServerForPath(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid request reached upstream")
	}, "/run/@cf/test/model")
	for _, value := range []string{"NaN", "Inf", "-Inf"} {
		w := uploadRequest(t, s, "/v1/images/generations", map[string]string{
			"model": "model", "prompt": "blue square", "n": value,
		}, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("n=%s: got %d: %s", value, w.Code, w.Body)
		}
	}
}

func imageFixture(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestImagesStandardResponseAndURL(t *testing.T) {
	picture := imageFixture(t)
	var calls int
	s := testServerForPath(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		data, _ := io.ReadAll(r.Body)
		raw := object(t, data)
		if raw["prompt"] != "blue square" || raw["width"] != float64(512) || raw["height"] != float64(256) || raw["n"] != nil || raw["model"] != nil {
			t.Error(raw)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"image": base64.StdEncoding.EncodeToString(picture)}})
	}, "/run/@cf/test/model")
	w := request(s, "/v1/images/generations", `{"model":"model","prompt":"blue square","size":"512x256","n":2,"response_format":"b64_json","output_format":"jpeg"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	items := object(t, w.Body.Bytes())["data"].([]any)
	if len(items) != 2 || calls != 2 {
		t.Fatal(items, calls)
	}
	decoded, err := base64.StdEncoding.DecodeString(items[0].(map[string]any)["b64_json"].(string))
	if err != nil || http.DetectContentType(decoded) != "image/jpeg" {
		t.Fatal(err)
	}
	w = request(s, "/v1/images/generations", `{"model":"model","prompt":"blue square","size":"512x256","response_format":"url"}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	link := object(t, w.Body.Bytes())["data"].([]any)[0].(map[string]any)["url"].(string)
	r := httptest.NewRequest("GET", link, nil)
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 200 || !bytes.Equal(out.Body.Bytes(), picture) {
		t.Fatal(out.Code, out.Body)
	}
	for key, file := range s.imageFiles.files {
		file.expires = time.Now().Add(-time.Minute)
		s.imageFiles.files[key] = file
	}
	out = httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 404 {
		t.Fatal("expired image served")
	}
}

func uploadRequest(t *testing.T, s *Server, path string, fields map[string]string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	form := multipart.NewWriter(&b)
	for name, value := range fields {
		form.WriteField(name, value)
	}
	for name, data := range files {
		part, err := form.CreateFormFile(name, "test.bin")
		if err != nil {
			t.Fatal(err)
		}
		part.Write(data)
	}
	form.Close()
	r := httptest.NewRequest("POST", path, &b)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Authorization", "Bearer client-secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestFlux2EditConvertsMultipart(t *testing.T) {
	picture := imageFixture(t)
	s := testServerForPath(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("prompt") != "make blue" || r.FormValue("width") != "512" {
			t.Error(r.Form)
		}
		file, _, err := r.FormFile("input_image_0")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if !bytes.Equal(data, picture) {
			t.Error("image changed")
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(picture)
	}, "/run/@cf/black-forest-labs/flux-2-klein-4b")
	w := uploadRequest(t, s, "/v1/images/edits", map[string]string{"model": "@cf/black-forest-labs/flux-2-klein-4b", "prompt": "make blue", "size": "512x512", "n": "1"}, map[string][]byte{"image": picture})
	if w.Code != 200 || len(object(t, w.Body.Bytes())["data"].([]any)) != 1 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestSpeechRequestAndBinary(t *testing.T) {
	for _, format := range []string{"mp3", "wav", "pcm", "opus", "aac", "flac"} {
		t.Run(format, func(t *testing.T) {
			audio := []byte{0, 128, 255, 42}
			s := testServerForPath(t, func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				raw := object(t, data)
				if raw["text"] != "Hello" || raw["speaker"] != "angus" || raw["input"] != nil || raw["voice"] != nil {
					t.Error(raw)
				}
				if format == "pcm" || format == "wav" {
					if raw["encoding"] != "linear16" || raw["sample_rate"] != float64(24000) {
						t.Error(raw)
					}
				}
				w.Header().Set("Content-Type", "audio/mpeg")
				w.Write(audio)
			}, "/run/@cf/deepgram/aura-1")
			body := fmt.Sprintf(`{"model":"@cf/deepgram/aura-1","input":"Hello","voice":"alloy","response_format":%q}`, format)
			w := request(s, "/v1/audio/speech", body)
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), audio) {
				t.Fatal(w.Code, w.Body)
			}
		})
	}
}

func TestSpeechDecodesBase64(t *testing.T) {
	s := testServerForPath(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if object(t, data)["prompt"] != "Hello" {
			t.Error(string(data))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"success":true,"result":{"audio":"/wAB"}}`)
	}, "/run/@cf/myshell-ai/melotts")
	w := request(s, "/v1/audio/speech", `{"model":"@cf/myshell-ai/melotts","input":"Hello","voice":"alloy"}`)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), []byte{255, 0, 1}) || w.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatal(w.Code, w.Body)
	}
}

func TestTranscriptionsStandardFormats(t *testing.T) {
	for _, format := range []string{"json", "text", "verbose_json", "srt", "vtt"} {
		for _, path := range []string{"/v1/audio/transcriptions", "/v1/audio/translations"} {
			t.Run(format+path, func(t *testing.T) {
				audio := []byte{0, 255, 128}
				s := testServerForPath(t, func(w http.ResponseWriter, r *http.Request) {
					data, _ := io.ReadAll(r.Body)
					raw := object(t, data)
					decoded, _ := base64.StdEncoding.DecodeString(raw["audio"].(string))
					if !bytes.Equal(decoded, audio) {
						t.Error("audio changed")
					}
					task := "transcribe"
					if strings.HasSuffix(path, "translations") {
						task = "translate"
					}
					if raw["task"] != task {
						t.Error(raw)
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"success":true,"result":{"text":"Hello","transcription_info":{"duration":1.5,"language":"en"},"segments":[{"start":0,"end":1.5,"text":"Hello"}]}}`)
				}, "/run/@cf/openai/whisper-large-v3-turbo")
				w := uploadRequest(t, s, path, map[string]string{"model": "@cf/openai/whisper-large-v3-turbo", "response_format": format}, map[string][]byte{"file": audio})
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body)
				}
				switch format {
				case "json":
					if object(t, w.Body.Bytes())["text"] != "Hello" {
						t.Fatal(w.Body)
					}
				case "text":
					if w.Body.String() != "Hello" {
						t.Fatal(w.Body)
					}
				case "verbose_json":
					raw := object(t, w.Body.Bytes())
					if raw["duration"] != 1.5 || len(raw["segments"].([]any)) != 1 {
						t.Fatal(raw)
					}
				case "srt":
					if !strings.Contains(w.Body.String(), "00:00:00,000 --> 00:00:01,500") {
						t.Fatal(w.Body)
					}
				case "vtt":
					if !strings.HasPrefix(w.Body.String(), "WEBVTT\n") || !strings.Contains(w.Body.String(), "00:00:01.500") {
						t.Fatal(w.Body)
					}
				}
			})
		}
	}
}

func TestRerankSortingAndDocumentIndices(t *testing.T) {
	s := testServerForPath(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		raw := object(t, data)
		if raw["query"] != "query" || len(raw["contexts"].([]any)) != 3 {
			t.Error(raw)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"result":{"response":[{"id":0,"score":0.2},{"id":2,"score":0.8},{"id":1,"score":0.9}]},"success":true}`)
	}, "/run/@cf/baai/bge-reranker-base")
	w := request(s, "/v1/rerank", `{"model":"@cf/baai/bge-reranker-base","query":"query","documents":["A","B","C"],"top_n":2}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	results := object(t, w.Body.Bytes())["results"].([]any)
	first := results[0].(map[string]any)
	if len(results) != 2 || first["index"] != float64(1) || first["document"].(map[string]any)["text"] != "B" {
		t.Fatal(results)
	}
}

func TestRemovedNativeEndpointAndValidation(t *testing.T) {
	s := testServer(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached upstream") })
	if w := request(s, "/ai/run/model", "{}"); w.Code != 404 {
		t.Fatalf("native endpoint still exposed: %d", w.Code)
	}
	for _, tc := range []struct{ path, body string }{
		{"/v1/images/generations", `{"model":"model","prompt":"hi","n":1.5}`},
		{"/v1/images/generations", `{"model":"model","prompt":"hi","size":"huge"}`},
		{"/v1/images/generations", `{"model":"model","prompt":"hi","response_format":"unknown"}`},
		{"/v1/audio/speech", `{"model":"model","input":"hi","speed":2}`},
		{"/v1/audio/speech", `{"model":"model","input":"hi","response_format":"bogus"}`},
		{"/v1/audio/transcriptions", `{"model":"model"}`},
		{"/v1/embeddings", `{"model":"model"}`},
	} {
		if w := request(s, tc.path, tc.body); w.Code != 400 {
			t.Error(tc.path, w.Code, w.Body)
		}
	}
}

func TestClassificationChatJSONAndStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		s := testServerForPath(t, func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(r.Body)
			if object(t, data)["text"] != "I love this" {
				t.Error(string(data))
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"success":true,"result":[{"label":"POSITIVE","score":0.99}]}`)
		}, "/run/@cf/huggingface/distilbert-sst-2-int8")
		w := request(s, "/v1/chat/completions", fmt.Sprintf(`{"model":"@cf/huggingface/distilbert-sst-2-int8","messages":[{"role":"user","content":"I love this"}],"stream":%t}`, stream))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "POSITIVE") {
			t.Fatal(w.Code, w.Body)
		}
		if stream && !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
			t.Fatal(w.Body)
		}
	}
}
