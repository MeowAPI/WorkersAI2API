package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const catalogFixture = `<html><div data-name="@cf/moonshotai/kimi-k2.7-code" data-models-cell data-facet-tasks="Text Generation" data-search="code &amp; tools"></div>
<div data-models-cell data-facet-tasks='Text Generation|Other' data-name='@cf/qwen/qwen3.8-27b'></div>
<div data-models-cell data-facet-tasks="Text Embeddings" data-name="@cf/baai/bge-m3"></div>
<div data-models-cell data-facet-tasks="Text-to-Image" data-name="@cf/black-forest-labs/flux"></div>
<div data-name="@cf/example/unrelated" data-facet-tasks="Text Generation"></div></html>`

func TestParseModelCatalog(t *testing.T) {
	routes, err := parseModelCatalog([]byte(catalogFixture))
	if err != nil {
		t.Fatal(err)
	}
	if routes["bge-m3"] != "@cf/baai/bge-m3" || routes["flux"] != "@cf/black-forest-labs/flux" {
		t.Fatal("non-text models missing", routes)
	}
	if len(routes) != 4 || routes["kimi-k2.7-code"] != "@cf/moonshotai/kimi-k2.7-code" || routes["qwen3.8-27b"] != "@cf/qwen/qwen3.8-27b" {
		t.Fatal(routes)
	}
	for _, body := range []string{`<html>challenge page</html>`, `<div data-models-cell data-name="bad" data-facet-tasks="Text Generation"></div>`, catalogFixture + `<div data-models-cell data-name="@cf/other/kimi-k2.7-code" data-facet-tasks="Text Generation"></div>`} {
		if _, err := parseModelCatalog([]byte(body)); err == nil {
			t.Errorf("invalid catalog accepted: %s", body)
		}
	}
}

func TestRefreshModelsAndShortRoutes(t *testing.T) {
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if object(t, body)["model"] != "@cf/moonshotai/kimi-k2.7-code" {
			t.Errorf("wrong route: %s", body)
		}
		io.WriteString(w, chatJSON)
	})
	calls := 0
	s.catalogClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != modelCatalogURL || r.Method != "GET" {
			t.Error("incorrect catalog request")
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("credentials leaked to docs")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(catalogFixture)), Header: make(http.Header)}, nil
	})}
	if err := s.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"chat", "responses", "anthropic", "gemini"} {
		path, body := streamRequest(protocol)
		body = strings.ReplaceAll(body, `"stream":true`, `"stream":false`)
		body = strings.ReplaceAll(body, `"model":"test"`, `"model":"kimi-k2.7-code"`)
		if protocol == "gemini" {
			path = "/v1beta/models/kimi-k2.7-code:generateContent"
		}
		w := request(s, path, body)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", protocol, w.Code, w.Body)
		}
	}
	for _, path := range []string{"/v1/models", "/v1/models/kimi-k2.7-code", "/v1beta/models", "/v1beta/models/kimi-k2.7-code"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer client-secret")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "kimi-k2.7-code") || strings.Contains(w.Body.String(), "@cf/") {
			t.Fatalf("catalog not short: %d %s", w.Code, w.Body)
		}
	}
	if calls != 1 {
		t.Error("requests should use cached catalog")
	}
	// A failed or malformed refresh must not remove working routes.
	for _, body := range []string{"", `<html>unavailable</html>`} {
		s.catalogClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if err := s.RefreshModels(context.Background()); err == nil {
			t.Error("bad refresh accepted")
		}
		if id, err := s.resolveModel("kimi-k2.7-code"); err != nil || id != "@cf/moonshotai/kimi-k2.7-code" {
			t.Fatal(id, err)
		}
	}
}

func TestCatalogFallback(t *testing.T) {
	s := testServer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected inference") })
	catalog, err := newCatalog()
	if err != nil || len(catalog.routes) < 60 || catalog.routes["kimi-k2.7-code"] != "@cf/moonshotai/kimi-k2.7-code" {
		t.Fatal(catalog, err)
	}
	if _, err := s.resolveModel("does-not-exist"); err == nil {
		t.Error("unknown short name accepted")
	}
	if id, err := s.resolveModel("@cf/future/new-model"); err != nil || id != "@cf/future/new-model" {
		t.Fatal(id, err)
	}
}

func TestConcurrentModelRefresh(t *testing.T) {
	s := testServer(t, func(http.ResponseWriter, *http.Request) {})
	s.catalogClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(catalogFixture))}, nil
	})}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if err := s.RefreshModels(context.Background()); err != nil {
					t.Error(err)
				}
				s.modelRoutes()
				s.resolveModel("kimi-k2.7-code")
			}
		}()
	}
	wg.Wait()
}

func TestModelSyncStopsWithContext(t *testing.T) {
	s := testServer(t, func(http.ResponseWriter, *http.Request) {})
	s.refreshInterval = 10 * time.Millisecond
	started := make(chan struct{}, 3)
	stopped := make(chan struct{})
	calls := 0
	s.catalogClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		started <- struct{}{}
		if calls == 1 {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(catalogFixture))}, nil
		}
		<-r.Context().Done()
		close(stopped)
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartModelSync(ctx)
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("sync did not refresh")
		}
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("refresh was not cancelled")
	}
}

func TestCatalogHTTPFailureRetainsRoutes(t *testing.T) {
	s := testServer(t, func(http.ResponseWriter, *http.Request) {})
	for _, status := range []int{301, 403, 500} {
		s.catalogClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("error"))}, nil
		})}
		if err := s.RefreshModels(context.Background()); err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) {
			t.Fatal(err)
		}
		if id, err := s.resolveModel("test"); err != nil || id != "@cf/test/model" {
			t.Fatal(id, err)
		}
	}
}

func TestCatalogPreservesTaskMetadata(t *testing.T) {
	routes, tasks, err := parseCatalogDetails([]byte(catalogFixture))
	if err != nil {
		t.Fatal(err)
	}
	if tasks[routes["bge-m3"]][0] != "Text Embeddings" || tasks[routes["flux"]][0] != "Text-to-Image" {
		t.Fatal(tasks)
	}
	s := testServer(t, func(http.ResponseWriter, *http.Request) {})
	s.catalog.mu.Lock()
	s.catalog.routes = routes
	s.catalog.tasks = tasks
	s.catalog.mu.Unlock()
	for _, path := range []string{"/v1/models/bge-m3", "/v1beta/models/bge-m3"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer client-secret")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		value := object(t, w.Body.Bytes())
		if w.Code != 200 || value["tasks"].([]any)[0] != "Text Embeddings" {
			t.Fatal(w.Code, value)
		}
		if methods, ok := value["supportedGenerationMethods"].([]any); ok && len(methods) != 0 {
			t.Fatal("embedding model advertised as chat", value)
		}
	}
	copied := s.modelTasks("@cf/baai/bge-m3")
	copied[0] = "changed"
	if s.modelTasks("@cf/baai/bge-m3")[0] != "Text Embeddings" {
		t.Fatal("shared task metadata mutated")
	}
}
