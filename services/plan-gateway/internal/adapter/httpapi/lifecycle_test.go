package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/httpapi"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l := listen(t)
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return l
}

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func TestServeReturnsWhenTheContextIsCancelled(t *testing.T) {
	// The shutdown path. A graceful shutdown that has never been exercised is
	// a graceful shutdown that hangs, and it is discovered during a deploy.
	server := &http.Server{Addr: freeAddr(t), ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- httpapi.Serve(ctx, server, time.Second, discard()) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

func TestServeDrainsAnInFlightRequest(t *testing.T) {
	// The point of a grace period. Cutting a request mid-flight looks to the
	// caller like an intermittent server error with no cause.
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		if _, err := w.Write([]byte("finished")); err != nil {
			panic(err)
		}
	})

	addr := freeAddr(t)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- httpapi.Serve(ctx, server, 5*time.Second, discard()) }()
	time.Sleep(50 * time.Millisecond)

	body := make(chan string, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/slow", nil)
		if err != nil {
			body <- "request error: " + err.Error()
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		defer func() {
			if cerr := resp.Body.Close(); cerr != nil {
				body <- "close error: " + cerr.Error()
			}
		}()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			body <- "read error: " + err.Error()
			return
		}
		body <- string(b)
	}()

	<-started
	cancel() // shut down while the request is still running

	select {
	case got := <-body:
		if !strings.Contains(got, "finished") {
			t.Fatalf("the in-flight request was cut off: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request never completed")
	}
	<-done
}

func TestServeReportsAListenFailure(t *testing.T) {
	// A port already in use must surface as an error, not as a process that
	// looks alive and serves nothing.
	l := listen(t)
	defer func() {
		if err := l.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	server := &http.Server{Addr: l.Addr().String(), ReadHeaderTimeout: time.Second}
	if err := httpapi.Serve(t.Context(), server, time.Second, discard()); err == nil {
		t.Fatal("binding an occupied port reported success")
	}
}
