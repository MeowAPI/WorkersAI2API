package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

func (s *Server) images(ctx context.Context, w *responseWriter, r *http.Request, in mediaInput) error {
	raw := in.fields
	if err := mediaAllowed(raw, "model", "prompt", "n", "size", "response_format", "user", "quality", "style", "output_format", "steps", "num_steps", "seed", "guidance", "negative_prompt", "strength"); err != nil {
		return err
	}
	_, id, err := s.mediaModel(raw, "flux-1-schnell", "Text-to-Image")
	if err != nil {
		return err
	}
	prompt, err := requireString(raw, "prompt")
	if err != nil {
		return invalid("%s", err)
	}
	count, err := mediaNumber(raw, "n", 1)
	if err != nil {
		return err
	}
	if count < 1 || count > 10 || math.Trunc(count) != count {
		return invalid("n must be an integer between 1 and 10")
	}
	format, err := mediaString(raw, "response_format", "b64_json")
	if err != nil {
		return err
	}
	if format != "b64_json" && format != "url" {
		return invalid("response_format must be b64_json or url")
	}
	outputFormat, err := mediaString(raw, "output_format", "")
	if err != nil {
		return err
	}
	if outputFormat != "" && outputFormat != "png" && outputFormat != "jpeg" {
		return invalid("output_format supports png and jpeg")
	}
	for _, key := range []string{"quality", "style"} {
		if value, ok := raw[key]; ok && value != "auto" && value != "standard" {
			return invalid("%s is not supported by this image model", key)
		}
	}
	payload := map[string]any{"prompt": prompt}
	name := shortName(id)
	flux1 := name == "flux-1-schnell"
	flux2 := strings.HasPrefix(name, "flux-2-")
	size, err := mediaString(raw, "size", "auto")
	if err != nil {
		return err
	}
	if size != "auto" && size != "" {
		dimensions := strings.Split(size, "x")
		if len(dimensions) != 2 {
			return invalid("size must be WIDTHxHEIGHT or auto")
		}
		width, e1 := strconv.Atoi(dimensions[0])
		height, e2 := strconv.Atoi(dimensions[1])
		if e1 != nil || e2 != nil || width < 256 || height < 256 || width > 2500 || height > 2500 {
			return invalid("invalid image size")
		}
		if flux1 {
			if width != 1024 || height != 1024 {
				return invalid("flux-1-schnell supports only 1024x1024 or auto")
			}
		} else {
			payload["width"] = width
			payload["height"] = height
		}
	}
	for _, key := range []string{"steps", "num_steps", "seed", "guidance", "strength"} {
		if raw[key] == nil {
			continue
		}
		n, err := mediaNumber(raw, key, 0)
		if err != nil {
			return err
		}
		if flux1 && key != "steps" && key != "num_steps" {
			return invalid("flux-1-schnell does not support %s", key)
		}
		target := key
		if key == "num_steps" && flux1 {
			target = "steps"
		}
		if key == "steps" && !flux1 && !flux2 && name != "lucid-origin" {
			target = "num_steps"
		}
		if payload[target] != nil {
			return invalid("conflicting steps and num_steps")
		}
		payload[target] = n
	}
	if raw["negative_prompt"] != nil {
		if flux1 || flux2 {
			return invalid("negative_prompt is unsupported by this model")
		}
		payload["negative_prompt"] = raw["negative_prompt"]
	}
	editing := r.URL.Path == "/v1/images/edits"
	var images [][]byte
	var mask []byte
	if editing {
		if len(in.files["image"]) > 0 && len(in.files["image[]"]) > 0 {
			return invalid("use image or image[], not both")
		}
		uploads := in.files["image"]
		if len(uploads) == 0 {
			uploads = in.files["image[]"]
		}
		if len(uploads) == 0 {
			return invalid("image file is required; use multipart/form-data")
		}
		if len(uploads) > 4 || (!flux2 && len(uploads) > 1) {
			return invalid("this model does not support that many input images")
		}
		for _, upload := range uploads {
			data, _, err := readUpload(map[string][]*multipart.FileHeader{"image": {upload}}, "image")
			if err != nil {
				return err
			}
			if err := validateImage(data); err != nil {
				return err
			}
			images = append(images, data)
		}
		if len(in.files["mask"]) > 0 {
			mask, _, err = readUpload(in.files, "mask")
			if err != nil {
				return err
			}
			if err := validateImage(mask); err != nil {
				return err
			}
		}
		if flux1 || strings.HasPrefix(name, "lucid-") || name == "phoenix-1.0" {
			return invalid("model %s does not support image edits", name)
		}
		if flux2 && len(mask) > 0 {
			return invalid("FLUX.2 edits do not support mask")
		}
		if !flux2 {
			payload["image_b64"] = base64.StdEncoding.EncodeToString(images[0])
			if len(mask) > 0 {
				payload["mask"] = byteNumbers(mask)
			}
		}
	} else if len(in.files) > 0 {
		return invalid("use /v1/images/edits for image uploads")
	}
	for key := range in.files {
		if key != "image" && key != "image[]" && key != "mask" {
			return invalid("unsupported file field %s", key)
		}
	}
	if name == "stable-diffusion-v1-5-inpainting" && (len(images) == 0 || len(mask) == 0) {
		return invalid("inpainting requires /v1/images/edits with image and mask")
	}
	result := make([]any, 0, int(count))
	for i := 0; i < int(count); i++ {
		var resp *http.Response
		if flux2 {
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			for key, value := range payload {
				if err := form.WriteField(key, fmt.Sprint(value)); err != nil {
					return err
				}
			}
			for j, data := range images {
				part, err := form.CreateFormFile(fmt.Sprintf("input_image_%d", j), "image.png")
				if err != nil {
					return err
				}
				if _, err = part.Write(data); err != nil {
					return err
				}
			}
			if err := form.Close(); err != nil {
				return err
			}
			resp, err = s.mediaUpstream(ctx, w, "/run/"+id, form.FormDataContentType(), body.Bytes())
		} else {
			resp, err = s.runModel(ctx, w, id, payload)
		}
		if err != nil {
			return err
		}
		data, mimeType, err := imageResult(resp)
		if err != nil {
			return err
		}
		if outputFormat != "" {
			data, mimeType, err = encodeImage(data, outputFormat)
			if err != nil {
				return err
			}
		}
		entry := map[string]any{}
		if format == "b64_json" {
			entry["b64_json"] = base64.StdEncoding.EncodeToString(data)
		} else {
			token, err := s.imageFiles.put(data, mimeType)
			if err != nil {
				return err
			}
			base := requestBaseURL(r)
			entry["url"] = strings.TrimRight(base, "/") + "/v1/files/" + token + "/content"
		}
		result = append(result, entry)
	}
	writeJSON(w, 200, map[string]any{"created": time.Now().Unix(), "data": result})
	return nil
}
func byteNumbers(data []byte) []int {
	out := make([]int, len(data))
	for i, b := range data {
		out[i] = int(b)
	}
	return out
}
func validateImage(data []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 25_000_000 {
		return invalid("image must be valid PNG or JPEG, at most 25 megapixels")
	}
	return nil
}
func imageResult(resp *http.Response) ([]byte, string, error) {
	var data []byte
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		value, err := modelResult(resp)
		if err != nil {
			return nil, "", err
		}
		obj, _ := value.(map[string]any)
		encoded, _ := obj["image"].(string)
		data, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) == 0 {
			return nil, "", upstreamInvalid("upstream image is missing or invalid base64")
		}
	} else {
		defer resp.Body.Close()
		var err error
		data, err = readLimited(resp.Body, maxMediaResponseBytes)
		if err != nil {
			return nil, "", upstreamInvalid("upstream image is truncated")
		}
	}
	if err := validateImage(data); err != nil {
		return nil, "", upstreamInvalid("upstream did not return a valid image")
	}
	return data, http.DetectContentType(data), nil
}
func encodeImage(data []byte, format string) ([]byte, string, error) {
	if err := validateImage(data); err != nil {
		return nil, "", err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", upstreamInvalid("cannot decode image")
	}
	var out bytes.Buffer
	mime := "image/png"
	if format == "jpeg" {
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 90})
		mime = "image/jpeg"
	} else {
		err = png.Encode(&out, img)
	}
	return out.Bytes(), mime, err
}

type storedImage struct {
	data    []byte
	mime    string
	expires time.Time
}
type imageStore struct {
	mu    sync.Mutex
	files map[string]storedImage
	size  int
}

func (store *imageStore) put(data []byte, mime string) (string, error) {
	if len(data) > 64<<20 {
		return "", upstreamInvalid("image exceeds cache limit")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := "file-" + hex.EncodeToString(random[:])
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.files == nil {
		store.files = map[string]storedImage{}
	}
	now := time.Now()
	for key, file := range store.files {
		if now.After(file.expires) {
			store.size -= len(file.data)
			delete(store.files, key)
		}
	}
	for store.size+len(data) > 64<<20 {
		oldest := ""
		var expiry time.Time
		for key, file := range store.files {
			if oldest == "" || file.expires.Before(expiry) {
				oldest = key
				expiry = file.expires
			}
		}
		store.size -= len(store.files[oldest].data)
		delete(store.files, oldest)
	}
	store.files[id] = storedImage{data, mime, now.Add(15 * time.Minute)}
	store.size += len(data)
	return id, nil
}
func (s *Server) imageContent(w *responseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/files/"), "/content")
	s.imageFiles.mu.Lock()
	file, ok := s.imageFiles.files[id]
	if ok && time.Now().After(file.expires) {
		s.imageFiles.size -= len(file.data)
		delete(s.imageFiles.files, id)
		ok = false
	}
	s.imageFiles.mu.Unlock()
	if !ok {
		writeError(w, "chat", 404, "image URL expired or not found")
		return
	}
	w.Header().Set("Content-Type", file.mime)
	w.Header().Set("Cache-Control", "private, max-age=0")
	w.Header().Set("Content-Length", strconv.Itoa(len(file.data)))
	if _, err := io.Copy(w, bytes.NewReader(file.data)); err != nil {
		panic(http.ErrAbortHandler)
	}
}
