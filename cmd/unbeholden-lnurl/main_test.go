package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServe_WaitsForInFlightRequests(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, srv, ln) }()
	go http.Get("http://" + ln.Addr().String())
	<-started

	cancel()
	select {
	case err := <-done:
		t.Fatalf("serve returned %v with a request in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
