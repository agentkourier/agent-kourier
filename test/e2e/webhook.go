package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// Response is what a webhook endpoint answered.
type Response struct {
	Status int
	Body   []byte // at most 1 MiB
}

// PostWebhook posts body as JSON to url, with a bearer token unless token is empty, for fixtures
// sent straight to Agent Kourier (a bad token, a malformed or oversized body). Any HTTP status is the
// Response; only a failure to get an answer is an error, and the token is never in it.
func PostWebhook(ctx context.Context, url, token string, body []byte) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("e2e: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("e2e: post webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Response{}, fmt.Errorf("e2e: read webhook answer: %w", err)
	}
	return Response{resp.StatusCode, b}, nil
}
