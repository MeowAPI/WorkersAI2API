package proxy

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const modelCatalogURL = "https://developers.cloudflare.com/workers-ai/models/"
const catalogTimeout = 30 * time.Second

// All model entries from modelCatalogURL, fetched 2026-09-30. Used only
// until a live refresh succeeds; never requires sending credentials to docs.
//
//go:embed models_snapshot.json
var modelSnapshot []byte

type modelCatalog struct {
	mu          sync.RWMutex
	refreshMu   sync.Mutex
	routes      map[string]string
	tasks       map[string][]string
	lastAttempt *time.Time
	lastSuccess *time.Time
	lastError   string
}

var fullModelID = regexp.MustCompile(`^@[a-zA-Z0-9_-]+/[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)
var shortModelID = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)
var catalogTag = regexp.MustCompile(`(?s)<[a-zA-Z][a-zA-Z0-9:-]*(?:[^"'<>]|"[^"]*"|'[^']*')*>`)
var catalogAttr = regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:.-]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>` + "`" + `]+)))?`)

func shortName(id string) string {
	if at := strings.LastIndexByte(id, '/'); at >= 0 {
		return id[at+1:]
	}
	return id
}

func catalogRoutes(ids []string) (map[string]string, error) {
	routes := make(map[string]string, len(ids))
	for _, id := range ids {
		if !fullModelID.MatchString(id) {
			return nil, fmt.Errorf("invalid catalog model ID %q", id)
		}
		name := shortName(id)
		if previous, ok := routes[name]; ok && previous != id {
			return nil, fmt.Errorf("ambiguous short model name %q (%s and %s)", name, previous, id)
		}
		routes[name] = id
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("catalog contains no models")
	}
	return routes, nil
}

// Read the same model cards the public catalog displays. Attribute order,
// whitespace, HTML entities and either quote style are supported. All task types
// are retained, including embeddings, speech and image models.
type catalogEntry struct {
	ID    string   `json:"id"`
	Tasks []string `json:"tasks"`
}

func parseModelCatalog(body []byte) (map[string]string, error) {
	routes, _, err := parseCatalogDetails(body)
	return routes, err
}

func parseCatalogDetails(body []byte) (map[string]string, map[string][]string, error) {
	var ids []string
	tasks := map[string][]string{}
	for _, tag := range catalogTag.FindAllString(string(body), -1) {
		if !strings.Contains(tag, "data-models-cell") {
			continue
		}
		attrs := map[string]string{}
		for _, match := range catalogAttr.FindAllStringSubmatch(tag, -1) {
			value := match[2]
			if value == "" {
				value = match[3]
			}
			if value == "" {
				value = match[4]
			}
			attrs[strings.ToLower(match[1])] = html.UnescapeString(value)
		}
		if _, ok := attrs["data-models-cell"]; !ok {
			continue
		}
		id := attrs["data-name"]
		ids = append(ids, id)
		for _, task := range strings.Split(attrs["data-facet-tasks"], "|") {
			if task = strings.TrimSpace(task); task != "" {
				tasks[id] = append(tasks[id], task)
			}
		}
	}
	routes, err := catalogRoutes(ids)
	return routes, tasks, err
}

func newCatalog() (*modelCatalog, error) {
	var entries []catalogEntry
	if err := json.Unmarshal(modelSnapshot, &entries); err != nil {
		return nil, err
	}
	var ids []string
	tasks := map[string][]string{}
	for _, entry := range entries {
		ids = append(ids, entry.ID)
		tasks[entry.ID] = entry.Tasks
	}
	routes, err := catalogRoutes(ids)
	if err != nil {
		return nil, err
	}
	return &modelCatalog{routes: routes, tasks: tasks}, nil
}

func (s *Server) modelRoutes() map[string]string {
	s.catalog.mu.RLock()
	defer s.catalog.mu.RUnlock()
	routes := make(map[string]string, len(s.catalog.routes))
	for name, id := range s.catalog.routes {
		routes[name] = id
	}
	return routes
}

func (s *Server) resolveModel(name string) (string, error) {
	s.catalog.mu.RLock()
	target, ok := s.catalog.routes[name]
	s.catalog.mu.RUnlock()
	if ok {
		return target, nil
	}
	// Keep full upstream IDs usable even before they enter the public catalog.
	if fullModelID.MatchString(name) {
		return name, nil
	}
	return "", fmt.Errorf("unknown model %q; see /v1/models", name)
}

// RefreshModels atomically replaces the live catalog only after a complete,
// valid fetch. Any network, parsing or collision error leaves old routes intact.
func (s *Server) RefreshModels(ctx context.Context) (resultErr error) {
	s.catalog.refreshMu.Lock()
	defer s.catalog.refreshMu.Unlock()
	now := time.Now()
	s.catalog.mu.Lock()
	s.catalog.lastAttempt = &now
	s.catalog.mu.Unlock()
	defer func() {
		s.catalog.mu.Lock()
		defer s.catalog.mu.Unlock()
		if resultErr != nil {
			s.catalog.lastError = resultErr.Error()
		} else {
			success := time.Now()
			s.catalog.lastSuccess = &success
			s.catalog.lastError = ""
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelCatalogURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "WorkersAI2API/1.0 (+https://developers.cloudflare.com/workers-ai/models/)")
	req.Header.Set("Accept", "text/html")
	resp, err := s.catalogClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch model catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("model catalog returned HTTP %d", resp.StatusCode)
	}
	body, err := readLimited(resp.Body, 8<<20)
	if err != nil {
		return err
	}
	routes, tasks, err := parseCatalogDetails(body)
	if err != nil {
		return err
	}
	s.catalog.mu.Lock()
	s.catalog.routes = routes
	s.catalog.tasks = tasks
	s.catalog.mu.Unlock()
	log.Printf("Model catalog refreshed: %d model routes", len(routes))
	return nil
}

// StartModelSync refreshes in the background at startup and every hour.
// The embedded snapshot serves requests immediately, even if the network fails.
func (s *Server) StartModelSync(ctx context.Context) {
	refresh := func() {
		if err := s.RefreshModels(ctx); err != nil && ctx.Err() == nil {
			log.Printf("Model catalog refresh failed; keeping existing routes: %v", err)
		}
	}
	go func() {
		refresh()
		ticker := time.NewTicker(s.refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

// modelTasks returns a copy so callers cannot mutate the shared catalog.
func (s *Server) modelTasks(id string) []string {
	s.catalog.mu.RLock()
	defer s.catalog.mu.RUnlock()
	return append([]string{}, s.catalog.tasks[id]...)
}
