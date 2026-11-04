package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bothought/sal/internal/agent"
)

// transient checks whether err asks the agent to retry.
func transient(err error) bool {
	var re *agent.RetryableError
	return errors.As(err, &re)
}

func TestConnectionResetIsTransient(t *testing.T) {
	cases := map[string]bool{
		`Post "https://x/v1/chat/completions": read tcp 1.2.3.4:5->6.7.8.9:443: read: connection reset by peer`: true,
		`read: connection reset by peer`:                       true,
		`dial tcp: connection refused`:                         true,
		`unexpected EOF`:                                       true,
		`write tcp: broken pipe`:                               true,
		`i/o timeout`:                                          true,
		`http2: server sent GOAWAY`:                            true,
		`context deadline exceeded`:                            false,
		`invalid character 'x' looking for beginning of value`: false,
		`x509: certificate signed by unknown authority`:        false,
	}
	for msg, want := range cases {
		if got := isTransientNetError(errors.New(msg)); got != want {
			t.Fatalf("isTransientNetError(%q) = %v, want %v", msg, got, want)
		}
	}
	if isTransientNetError(nil) {
		t.Fatal("nil is not a transient error")
	}
	var ne net.Error = &net.DNSError{Err: "boom", IsTimeout: true}
	if !isTransientNetError(ne) {
		t.Fatal("a timeout must be retried")
	}
	if !isTransientNetError(fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF)) {
		t.Fatal("a wrapped unexpected EOF must be retried")
	}
}

func TestTransientHTTPStatuses(t *testing.T) {
	for code, want := range map[int]bool{
		400: false, 401: false, 403: false, 404: false, 422: false,
		408: true, 429: true, 500: true, 502: true, 503: true, 504: true, 529: true,
	} {
		if got := isTransientStatus(code); got != want {
			t.Fatalf("isTransientStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestRetryAfterParsing(t *testing.T) {
	if got := retryAfter("3"); got != 3*time.Second {
		t.Fatalf("seconds form = %v", got)
	}
	if got := retryAfter(""); got != 0 {
		t.Fatalf("empty = %v", got)
	}
	if got := retryAfter("not-a-date"); got != 0 {
		t.Fatalf("garbage = %v", got)
	}
	when := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryAfter(when); got <= 0 || got > 2*time.Second {
		t.Fatalf("date form = %v", got)
	}
}

func TestRateLimitCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	_, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	var re *agent.RetryableError
	if !errors.As(err, &re) {
		t.Fatalf("429 should be retryable: %v", err)
	}
	if re.RetryAfter != 2*time.Second {
		t.Fatalf("Retry-After not honored: %v", re.RetryAfter)
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("the message should keep the status and body: %v", err)
	}
}

func TestServerErrorIsRetryableButBadRequestIsNot(t *testing.T) {
	code := http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", code)
	}))
	defer srv.Close()
	o := NewFromEnv("k", srv.URL, "m")

	_, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if !transient(err) {
		t.Fatalf("500 should be retryable: %v", err)
	}

	code = http.StatusBadRequest
	_, err = o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if transient(err) {
		t.Fatalf("400 must not be retried: %v", err)
	}
}

func TestStreamFailureMidwayIsRetryable(t *testing.T) {
	// The server sends a valid chunk, then drops the connection without the
	// terminating [DONE], which is what a reset mid-stream looks like.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close() // reset without a clean end
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	msg, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if err == nil {
		t.Fatalf("a truncated stream must be an error, got %q", msg.Content)
	}
	if !transient(err) {
		t.Fatalf("a truncated stream should be retryable: %v", err)
	}
}

func TestAbortIsNotRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := o.Stream(ctx, []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if transient(err) {
		t.Fatalf("an aborted request must not be retried: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestClientReusesConnections(t *testing.T) {
	var conns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	for i := 0; i < 3; i++ {
		if _, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&conns); got != 1 {
		t.Fatalf("expected one reused connection, got %d", got)
	}
}

func TestEmptyStreamIsRetryable(t *testing.T) {
	// Headers, then a close with no chunks at all: a reset that would otherwise
	// look like an empty answer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	_, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if !transient(err) {
		t.Fatalf("an empty stream should be retryable: %v", err)
	}
}

func TestCompleteStreamIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"all done\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	msg, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "all done" {
		t.Fatalf("content = %q", msg.Content)
	}
}

func TestStalledServerTimesOutOnHeaders(t *testing.T) {
	// A server that accepts the request and never answers: the header deadline
	// must end the wait, and the failure must be retryable.
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m").WithHeaderTimeout(50 * time.Millisecond)
	start := time.Now()
	_, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the header deadline was not honored: %v", elapsed)
	}
	if !transient(err) {
		t.Fatalf("a stalled server should be retried: %v", err)
	}
}

func TestSlowStreamIsNotCutOffByTheHeaderTimeout(t *testing.T) {
	// The header deadline must not bound the body: a server that answers at once
	// and then streams slowly must complete.
	const chunks = 4
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush() // headers are out; the body takes its time
		for i := 0; i < chunks; i++ {
			time.Sleep(60 * time.Millisecond)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"c%d\"}}]}\n\n", i)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m").WithHeaderTimeout(20 * time.Millisecond)
	msg, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if err != nil {
		t.Fatalf("a slow stream must not be cut off: %v", err)
	}
	if msg.Content != "c0c1c2c3" {
		t.Fatalf("content = %q, want the whole stream", msg.Content)
	}
}

func TestResetThenFullAnswerThroughTheAgentLoop(t *testing.T) {
	// The reported failure, end to end at the provider boundary: the first
	// connection is reset, the retry gets a complete streamed answer.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"all good\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	_, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if !transient(err) {
		t.Fatalf("the reset should be classified as retryable: %v", err)
	}

	msg, err := o.Stream(context.Background(), []agent.Message{agent.NewUserMessage("hi")}, nil, nil)
	if err != nil {
		t.Fatalf("the retry should succeed: %v", err)
	}
	if msg.Content != "all good" {
		t.Fatalf("content = %q", msg.Content)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server hits = %d, want 2", got)
	}
}

func TestClassifyRequestErrorMarksResets(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	err := classifyRequestError("https://x/v1/chat/completions", reset)
	if !transient(err) {
		t.Fatalf("a reset must be retryable: %v", err)
	}
	if !strings.Contains(err.Error(), "https://x/v1/chat/completions") {
		t.Fatalf("the message should name the endpoint: %v", err)
	}
	permanent := classifyRequestError("https://x/v1", errors.New("unsupported protocol scheme"))
	if transient(permanent) {
		t.Fatalf("a permanent failure must not be retryable: %v", permanent)
	}
}

func TestContextWindowFromModelMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"other","context_length":1000},`+
			`{"id":"m","top_provider":{"context_length":32000}}]}`)
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	got, ok := o.ContextWindow(context.Background())
	if !ok || got != 32000 {
		t.Fatalf("window = %d %v", got, ok)
	}
	// The lookup is memoized, so a second call does not depend on the server.
	srv.Close()
	if got, ok := o.ContextWindow(context.Background()); !ok || got != 32000 {
		t.Fatalf("cached window = %d %v", got, ok)
	}
}

func TestContextWindowIsNotPublished(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	if n, ok := o.ContextWindow(context.Background()); ok {
		t.Fatalf("a missing limit is not a failure to report: %d", n)
	}
}

func TestModelSwitchForgetsTheCachedWindow(t *testing.T) {
	o := NewFromEnv("k", "http://127.0.0.1:1", "a")
	// Nothing listens, so the lookup fails; the point is that the cached "not
	// published" answer is discarded when the model changes.
	if _, ok := o.ContextWindow(context.Background()); ok {
		t.Skip("a lookup unexpectedly succeeded")
	}
	o.SetModelName("b")
	if o.limit != nil {
		t.Fatal("switching models should forget the cached window")
	}
}

// TestContextWindowFromLimitObject covers the catalogue form a gateway such as
// opencode.ai answers with, {"limit":{"context":N,"output":N}}, which nests the
// figure one level away from context_length.
func TestContextWindowFromLimitObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[`+
			`{"id":"other","limit":{"context":200000,"output":8192}},`+
			`{"id":"m","context_length":0,"limit":{"context":1000000,"output":384000}}]}`)
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	got, ok := o.ContextWindow(context.Background())
	if !ok || got != 1000000 {
		t.Fatalf("window = %d %v, want 1000000", got, ok)
	}
}

// TestContextWindowSurvivesALongCatalogue guards the size of the metadata read:
// a catalogue bigger than a failure-message body must still decode, because a
// truncated body is indistinguishable from "no limit published" and would drop
// resolution to the built-in table.
func TestContextWindowSurvivesALongCatalogue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		var b strings.Builder
		b.WriteString(`{"object":"list","data":[`)
		// Roughly 32 KiB of unrelated models, then the one asked for: enough to
		// overflow maxErrorBody but not a real catalogue.
		for i := 0; i < 200; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":"filler-%03d","object":"model","owned_by":"gateway",`+
				`"description":"%s","limit":{"context":128000,"output":8192}}`, i, strings.Repeat("x", 100))
		}
		b.WriteString(`,{"id":"m","limit":{"context":1000000}}]}`)
		_, _ = io.WriteString(w, b.String())
	}))
	defer srv.Close()

	o := NewFromEnv("k", srv.URL, "m")
	got, ok := o.ContextWindow(context.Background())
	if !ok || got != 1000000 {
		t.Fatalf("window = %d %v, want 1000000", got, ok)
	}
}
