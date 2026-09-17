package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRunWaitsForInFlightRequestDuringShutdown(t *testing.T) {
	requestStarted := make(chan struct{})
	finishRequest := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-finishRequest
		_, _ = io.WriteString(w, "completed")
	})

	listener := newListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, listener, handler, 5*time.Second)
	}()

	responseErr := make(chan error, 1)
	responseBody := make(chan string, 1)
	go func() {
		res, err := http.Post("http://"+listener.Addr().String(), "text/plain", strings.NewReader(""))
		if err != nil {
			responseErr <- err
			return
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			responseErr <- err
			return
		}
		responseBody <- string(body)
		responseErr <- nil
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}

	cancel()
	select {
	case err := <-runErr:
		t.Fatalf("server exited before request completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(finishRequest)
	if err := <-responseErr; err != nil {
		t.Fatalf("in-flight response failed: %v", err)
	}
	if body := <-responseBody; body != "completed" {
		t.Fatalf("response body = %q, want completed", body)
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run() error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not exit after request completed")
	}
}

func TestRunReturnsErrorWhenDrainTimesOut(t *testing.T) {
	requestStarted := make(chan struct{})
	finishRequest := make(chan struct{})
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(requestStarted)
		<-finishRequest
	})

	listener := newListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, listener, handler, 50*time.Millisecond)
	}()

	clientDone := make(chan struct{})
	go func() {
		_, _ = http.Post("http://"+listener.Addr().String(), "text/plain", strings.NewReader(""))
		close(clientDone)
	}()

	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}

	cancel()
	select {
	case err := <-runErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("run() error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not fail after drain timeout")
	}

	close(finishRequest)
	<-clientDone
}

func newListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}
