// Package httpx is the shared net/http plumbing for the vendor adapters:
// JSON requests, status mapping, and section 10's retry policy — retries
// only before the first response byte, for connection errors, 429 and 5xx,
// at most 4 attempts, exponential backoff with full jitter (base 500 ms,
// cap 20 s) honouring Retry-After, and no retry once a stream has begun.
// Adapters share this one client alongside the shared SSE decoder.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Defaults from section 10's retry rules.
const (
	DefaultBase       = 500 * time.Millisecond
	DefaultCap        = 20 * time.Second
	DefaultMaxAttempt = 4
)

// Error is a failed call: an HTTP status the adapter did not have to be
// told about, or the transport error that exhausted the retry budget.
// Type and Message are filled in from the vendor's error body by the
// adapter when the body parses.
type Error struct {
	Status    int
	Type      string
	Message   string
	Retryable bool
}

func (e *Error) Error() string {
	switch {
	case e.Status > 0 && e.Message != "":
		return fmt.Sprintf("http status %d (%s): %s", e.Status, e.Type, e.Message)
	case e.Status > 0:
		return fmt.Sprintf("http status %d", e.Status)
	default:
		return e.Message
	}
}

// Client performs retried JSON calls and opens streams. The zero value is
// not usable; call New.
type Client struct {
	HTTP *http.Client
	// Sleep waits for d until ctx ends; tests replace it to run instantly.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter picks the actual delay within [0, upTo]; tests replace it.
	Jitter func(upTo time.Duration) time.Duration
	Base   time.Duration
	Cap    time.Duration
	// MaxAttempts counts the initial attempt (section 10: at most 4).
	MaxAttempts int
}

// New returns a Client with section 10's defaults.
func New() *Client {
	return &Client{
		HTTP:        &http.Client{},
		Sleep:       sleepCtx,
		Jitter:      fullJitter,
		Base:        DefaultBase,
		Cap:         DefaultCap,
		MaxAttempts: DefaultMaxAttempt,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func fullJitter(upTo time.Duration) time.Duration {
	if upTo <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(upTo) + 1))
}

// Result is one completed JSON call.
type Result struct {
	Status int
	Body   []byte
	Header http.Header
}

// Conn is one opened stream: the status and headers are available before
// the first byte is read, and Body must be closed by the caller.
type Conn struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

// JSON marshals body once, POSTs it to url with hdr, applies the retry
// policy, and returns the result. Any status the policy did not consume is
// returned for the adapter to map (only 429/5xx are retried).
func (c *Client) JSON(ctx context.Context, url string, hdr map[string]string, body any) (Result, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, fmt.Errorf("httpx: encode request: %w", err)
	}
	resp, err := c.do(ctx, url, hdr, raw)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("httpx: read response: %w", err)
	}
	return Result{Status: resp.StatusCode, Body: data, Header: resp.Header}, nil
}

// Stream POSTs like JSON but hands back the live response body on the
// first 2xx. The retry policy cannot run past this point: once the caller
// reads the stream, a failure is the caller's to surface (section 10).
// A non-2xx arrives as a Result-shaped Conn whose Body is already closed,
// so adapters map it exactly like a JSON error; the caller closes every
// other Body.
func (c *Client) Stream(ctx context.Context, url string, hdr map[string]string, body any) (Conn, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return Conn{}, fmt.Errorf("httpx: encode request: %w", err)
	}
	resp, err := c.do(ctx, url, hdr, raw)
	if err != nil {
		return Conn{}, err
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return Conn{}, &StreamError{Status: resp.StatusCode, Header: resp.Header, Body: data}
	}
	return Conn{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

// StreamError is a non-2xx stream open: status, headers and the fully
// read error body, so the adapter never has to touch a half-open stream.
type StreamError struct {
	Status int
	Header http.Header
	Body   []byte
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("http status %d", e.Status)
}

// do runs the retry loop: connection errors, 429 and 5xx, at most
// MaxAttempts attempts, honouring Retry-After. A 4xx other than 429, or
// any 2xx, returns immediately — the first response byte ends the policy.
func (c *Client) do(ctx context.Context, url string, hdr map[string]string, raw []byte) (*http.Response, error) {
	attempts := c.MaxAttempts
	if attempts <= 0 {
		attempts = DefaultMaxAttempt
	}
	var last error
	var retryAfter time.Duration
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := c.Sleep(ctx, c.backoff(attempt, retryAfter)); err != nil {
				return nil, err
			}
			retryAfter = 0
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = &Error{Message: "transport: " + err.Error(), Retryable: true}
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
			last = &Error{Status: resp.StatusCode, Retryable: true}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			continue
		}
		return resp, nil // first response byte reached: no more retries
	}
	return nil, last
}

// backoff computes the delay before attempt (1-based): full jitter over
// min(cap, base*2^attempt), raised to Retry-After when the server asked
// for longer.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	base := c.Base
	if base <= 0 {
		base = DefaultBase
	}
	capD := c.Cap
	if capD <= 0 {
		capD = DefaultCap
	}
	upTo := base
	for i := 1; i < attempt && upTo < capD; i++ {
		upTo *= 2
	}
	if upTo > capD {
		upTo = capD
	}
	d := c.Jitter(upTo)
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

// parseRetryAfter reads both forms: delta-seconds and an HTTP date.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
