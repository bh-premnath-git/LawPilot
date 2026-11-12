package contextengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The provider is fixed by deployment configuration, never by request input.
// All 1536 dimensions and model profile must match; there is no fake fallback.
type Embedder struct {
	URL, Model, Revision, Key, Profile string
	Client                             *http.Client
}

func configuredEmbedder() (*Embedder, error) {
	endpoint := os.Getenv("EMBEDDING_BASE_URL")
	if endpoint == "" {
		return nil, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && os.Getenv("APP_ENV") == "development")) {
		return nil, errors.New("invalid embedding endpoint")
	}
	model, revision := os.Getenv("EMBEDDING_MODEL"), os.Getenv("EMBEDDING_REVISION")
	if model == "" || revision == "" {
		return nil, errors.New("embedding model/revision required")
	}
	return &Embedder{URL: strings.TrimRight(endpoint, "/") + "/embeddings", Model: model, Revision: revision, Key: os.Getenv("EMBEDDING_API_KEY"), Profile: digest(endpoint + "|" + model + "|" + revision + "|1536"), Client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect denied") }}}, nil
}
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	if len(texts) < 1 || len(texts) > 128 {
		return nil, errors.New("invalid embedding batch")
	}
	body, _ := json.Marshal(map[string]any{"model": e.Model, "input": texts, "dimensions": 1536, "encoding_format": "float"})
	req, err := http.NewRequestWithContext(ctx, "POST", e.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.Key != "" {
		req.Header.Set("Authorization", "Bearer "+e.Key)
	}
	response, err := e.Client.Do(req)
	if err != nil {
		return nil, errors.New("embedding provider unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, errors.New("embedding provider rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024+1))
	if err != nil || len(raw) > 8*1024*1024 {
		return nil, errors.New("invalid embedding response")
	}
	var result struct {
		Model string `json:"model"`
		Data  []struct {
			Index     int        `json:"index"`
			Embedding []*float64 `json:"embedding"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Model != e.Model || len(result.Data) != len(texts) {
		return nil, errors.New("embedding profile mismatch")
	}
	values := make([][]float64, len(texts))
	for _, row := range result.Data {
		if row.Index < 0 || row.Index >= len(values) || values[row.Index] != nil || len(row.Embedding) != 1536 {
			return nil, errors.New("embedding shape mismatch")
		}
		norm := 0.0
		value := make([]float64, len(row.Embedding))
		for i, pointer := range row.Embedding {
			if pointer == nil {
				return nil, errors.New("invalid embedding coefficient")
			}
			v := *pointer
			value[i] = v
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, errors.New("nonfinite embedding")
			}
			norm += v * v
		}
		if norm == 0 || math.IsInf(norm, 0) {
			return nil, errors.New("invalid embedding norm")
		}
		for i := range value {
			value[i] = value[i] / math.Sqrt(norm)
		}
		values[row.Index] = value
	}
	return values, nil
}
func vectorJSON(value []float64) string { body, _ := json.Marshal(value); return string(body) }
