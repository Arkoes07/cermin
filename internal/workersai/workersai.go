// Package workersai is a small client for Cloudflare's Workers AI REST API:
// only the embedding call Cermin uses.
package workersai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

// APIURL is the Cloudflare API's address.
const APIURL = "https://api.cloudflare.com/client/v4"

// DefaultModel is the embedding model unless configured otherwise.
const DefaultModel = "@cf/qwen/qwen3-embedding-0.6b"

const (
	// maxBatch is the most texts one call takes.
	maxBatch = 32
	// timeout bounds every call.
	timeout = 10 * time.Second
)

// Client embeds texts with one Workers AI embedding model.
type Client struct {
	apiURL    string
	accountID string
	apiToken  string
	model     string
}

// New returns a client that embeds with model on the Cloudflare account
// accountID, using the API token apiToken. apiURL is APIURL, or a fake's
// address in tests.
func New(apiURL, accountID, apiToken, model string) *Client {
	return &Client{apiURL: apiURL, accountID: accountID, apiToken: apiToken, model: model}
}

// Model returns the embedding model c embeds with.
func (c *Client) Model() string {
	return c.model
}

// Embed returns the vector of each of texts, in the same order: of the raw
// text, sent as documents, so with no task instruction. It asks for at most
// maxBatch vectors a call.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, 0, len(texts))
	for batch := range slices.Chunk(texts, maxBatch) {
		batchVectors, err := c.embedBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		vectors = append(vectors, batchVectors...)
	}
	return vectors, nil
}

// embedBatch returns the vector of each of texts, at most maxBatch, in one
// call. The token goes in a header, never in the URL, so no error carries it.
func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{"text": texts})
	if err != nil {
		return nil, fmt.Errorf("workersai: embed: encoding request: %w", err)
	}

	endpoint := c.apiURL + "/accounts/" + c.accountID + "/ai/run/" + c.model
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("workersai: embed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("workersai: embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, readAPIError(resp)
	}
	var r struct {
		Result struct {
			Data [][]float32 `json:"data"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("workersai: embed: decoding response: %w", err)
	}
	if len(r.Result.Data) != len(texts) {
		return nil, fmt.Errorf("workersai: embed: got %d vectors for %d texts", len(r.Result.Data), len(texts))
	}
	return r.Result.Data, nil
}

// readAPIError returns the error of resp, a response other than 200: the
// API's first error message and code, such as "Authentication error (code
// 10000)", or its body as it is if it has none.
func readAPIError(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("workersai: embed: status %d: reading reason: %w", resp.StatusCode, err)
	}
	var r struct {
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &r); err != nil || len(r.Errors) == 0 {
		return fmt.Errorf("workersai: embed: status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	e := r.Errors[0]
	return fmt.Errorf("workersai: embed: status %d: %s (code %d)", resp.StatusCode, e.Message, e.Code)
}
