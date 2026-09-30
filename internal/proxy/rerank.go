package proxy

import (
	"context"
	"math"
	"sort"
)

func (s *Server) rerank(ctx context.Context, w *responseWriter, in mediaInput) error {
	raw := in.fields
	if err := mediaAllowed(raw, "model", "query", "documents", "top_n", "return_documents"); err != nil {
		return err
	}
	name, id, err := s.mediaModel(raw, "bge-reranker-base", "")
	if err != nil {
		return err
	}
	if shortName(id) != "bge-reranker-base" {
		return invalid("model %s is not a supported reranker", name)
	}
	query, err := requireString(raw, "query")
	if err != nil {
		return invalid("%s", err)
	}
	docs, err := requireJSONArray(raw, "documents")
	if err != nil {
		return invalid("%s", err)
	}
	contexts := make([]any, len(docs))
	for i, doc := range docs {
		switch v := doc.(type) {
		case string:
			contexts[i] = map[string]any{"text": v}
		case map[string]any:
			if _, ok := v["text"].(string); !ok {
				return invalid("documents[%d].text is required", i)
			}
			contexts[i] = map[string]any{"text": v["text"]}
		default:
			return invalid("documents[%d] must be text or an object with text", i)
		}
	}
	top, err := mediaNumber(raw, "top_n", float64(len(docs)))
	if err != nil {
		return err
	}
	if top < 1 || top > float64(len(docs)) || math.Trunc(top) != top {
		return invalid("top_n must be between 1 and the document count")
	}
	returnDocs := true
	if v, ok := raw["return_documents"]; ok {
		b, ok := v.(bool)
		if !ok {
			return invalid("return_documents must be boolean")
		}
		returnDocs = b
	}
	resp, err := s.runModel(ctx, w, id, map[string]any{"query": query, "contexts": contexts})
	if err != nil {
		return err
	}
	value, err := modelResult(resp)
	if err != nil {
		return err
	}
	if obj, ok := value.(map[string]any); ok {
		value = obj["response"]
	}
	entries, ok := value.([]any)
	if !ok {
		return upstreamInvalid("invalid rerank response")
	}
	results := make([]map[string]any, 0, len(entries))
	seen := map[int]bool{}
	for _, v := range entries {
		entry, ok := v.(map[string]any)
		if !ok {
			return upstreamInvalid("invalid rerank entry")
		}
		score, ok := entry["score"].(float64)
		if !ok {
			return upstreamInvalid("rerank entry has no score")
		}
		index, ok := entry["id"].(float64)
		if !ok {
			index, ok = entry["index"].(float64)
		}
		if !ok || index < 0 || index >= float64(len(docs)) || math.Trunc(index) != index || seen[int(index)] {
			return upstreamInvalid("invalid rerank document index")
		}
		seen[int(index)] = true
		result := map[string]any{"index": int(index), "relevance_score": score}
		if returnDocs {
			result["document"] = contexts[int(index)]
		}
		results = append(results, result)
	}
	if len(results) != len(docs) {
		return upstreamInvalid("upstream did not score every document")
	}
	sort.SliceStable(results, func(i, j int) bool {
		return results[i]["relevance_score"].(float64) > results[j]["relevance_score"].(float64)
	})
	writeJSON(w, 200, map[string]any{"model": name, "results": results[:int(top)]})
	return nil
}
