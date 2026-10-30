package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testClient returns a Client with deterministic timing: jitter always
// picks the top of the range, and Sleep records instead of waiting.
func testClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New()
	c.HTTP = srv.Client()
	var sleeps []time.Duration
	c.Jitter = func(upTo time.Duration) time.Duration { return upTo }
	c.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return ctx.Err()
	}
	return c, srv, &sleeps
}

func TestRetries429And5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	c, srv, sleeps := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	}))

	res, err := c.JSON(context.Background(), srv.URL, nil, map[string]any{"a": 1})
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if res.Status != 200 || string(res.Body) != `{"ok":true}` {
		t.Fatalf("result = %+v", res)
	}
	if calls.Load() != 3 {
		t.Fatalf("server saw %d calls, want 3", calls.Load())
	}
	if len(*sleeps) != 2 {
		t.Fatalf("slept %d times, want 2: %v", len(*sleeps), *sleeps)
	}
	// First retry honours Retry-After: 1s beats the jittered 500ms.
	if (*sleeps)[0] != time.Second {
		t.Errorf("first backoff = %v, want 1s (Retry-After)", (*sleeps)[0])
	}
	// Second retry: exponential (500ms -> 1s), jittered to the top.
	if (*sleeps)[1] != time.Second {
		t.Errorf("second backoff = %v, want 1s", (*sleeps)[1])
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	c, srv, sleeps := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	_, err := c.JSON(context.Background(), srv.URL, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var he *Error
	if !errors.As(err, &he) || he.Status != 500 || !he.Retryable {
		t.Fatalf("err = %#v, want retryable 500", err)
	}
	if calls.Load() != DefaultMaxAttempt {
		t.Fatalf("server saw %d calls, want %d", calls.Load(), DefaultMaxAttempt)
	}
	if len(*sleeps) != DefaultMaxAttempt-1 {
		t.Fatalf("slept %d times, want %d", len(*sleeps), DefaultMaxAttempt-1)
	}
}

func TestFourOhFourIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c, srv, sleeps := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"nope"}`))
	}))
	res, err := c.JSON(context.Background(), srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if res.Status != 404 || string(res.Body) != `{"error":"nope"}` {
		t.Fatalf("result = %+v", res)
	}
	if calls.Load() != 1 || len(*sleeps) != 0 {
		t.Fatalf("calls=%d sleeps=%d, want 1 and 0", calls.Load(), len(*sleeps))
	}
}

// flakyTransport fails the first n attempts at the transport level, then
// delegates — section 10 counts connection errors as retryable.
type flakyTransport struct {
	next   http.RoundTripper
	remain atomic.Int32
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.remain.Add(-1) >= 0 {
		return nil, errors.New("connection reset")
	}
	return f.next.RoundTrip(r)
}

func TestConnectionErrorsAreRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New()
	var sleeps []time.Duration
	c.Jitter = func(upTo time.Duration) time.Duration { return upTo }
	c.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	ft := &flakyTransport{next: srv.Client().Transport}
	ft.remain.Store(2)
	c.HTTP = &http.Client{Transport: ft}

	if _, err := c.JSON(context.Background(), srv.URL, nil, nil); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if len(sleeps) != 2 {
		t.Fatalf("slept %d times, want 2", len(sleeps))
	}
}

func TestCancelDuringBackoffReturnsContextError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New()
	c.HTTP = srv.Client()
	// The first attempt gets a 503; the backoff that follows is when the
	// caller cancels, which must abort the retry loop immediately.
	c.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return ctx.Err()
	}

	_, err := c.JSON(ctx, srv.URL, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestStreamOpensOnceWithHeaders(t *testing.T) {
	c, srv, sleeps := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-9")
		w.Write([]byte("data: hi\n\n"))
	}))
	conn, err := c.Stream(context.Background(), srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer conn.Body.Close()
	if conn.Header.Get("X-Request-Id") != "req-9" {
		t.Errorf("headers = %v", conn.Header)
	}
	body, _ := io.ReadAll(conn.Body)
	if string(body) != "data: hi\n\n" {
		t.Errorf("body = %q", body)
	}
	if len(*sleeps) != 0 {
		t.Errorf("stream retried: %v", *sleeps)
	}
}

func TestStream4xxIsStreamErrorWithBody(t *testing.T) {
	c, srv, sleeps := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	_, err := c.Stream(context.Background(), srv.URL, nil, nil)
	var se *StreamError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StreamError", err)
	}
	if se.Status != 401 || string(se.Body) != `{"error":"bad key"}` {
		t.Fatalf("stream error = %+v", se)
	}
	if len(*sleeps) != 0 {
		t.Fatalf("unauthorized stream retried: %v", *sleeps)
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	c := New()
	c.Jitter = func(upTo time.Duration) time.Duration { return upTo }
	want := []time.Duration{
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		20 * time.Second,
		20 * time.Second,
	}
	for i, w := range want {
		if got := c.backoff(i+1, 0); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("3"); got != 3*time.Second {
		t.Errorf("seconds = %v", got)
	}
	if got := parseRetryAfter("nonsense"); got != 0 {
		t.Errorf("nonsense = %v", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("empty = %v", got)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("past date = %v, want 0", got)
	}
}

func TestErrorMessageFormatting(t *testing.T) {
	e := &Error{Status: 429, Type: "rate_limit", Message: "slow down"}
	if got := e.Error(); !strings.Contains(got, "429") || !strings.Contains(got, "slow down") {
		t.Errorf("Error() = %q", got)
	}
	e = &Error{Message: "transport: boom"}
	if got := e.Error(); got != "transport: boom" {
		t.Errorf("Error() = %q", got)
	}
}
