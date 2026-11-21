package cliapp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExportListenerFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	requests, stopRequests := context.WithCancel(t.Context())
	runtime := ExportRuntime{cancel: stopRequests}
	t.Cleanup(func() { _ = runtime.Shutdown(true, time.Second) })
	for range 2 {
		server := httptest.NewUnstartedServer(nil)
		server.Config.BaseContext = func(net.Listener) context.Context { return requests }
		runtime.Endpoints = append(runtime.Endpoints, &endpoint{
			info:     EndpointInfo{Listen: server.Listener.Addr().String()},
			Listener: server.Listener,
			Server:   server.Config,
		})
	}
	failed, live := runtime.Endpoints[0], runtime.Endpoints[1]
	started := make(chan context.Context, 1)
	live.Server.(*http.Server).Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- r.Context()
		<-r.Context().Done()
	})
	exited := make(chan error, 1)
	go func() { exited <- runtime.Run(ctx) }()
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+live.info.Listen, nil)
		if res, err := http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
		}
	}()
	var active context.Context
	select {
	case active = <-started:
	case <-ctx.Done():
		t.Fatal("second listener did not accept a request")
	}
	_ = failed.Listener.Close()
	select {
	case err := <-exited:
		if err == nil || !strings.Contains(err.Error(), failed.info.Listen) {
			t.Fatalf("missing listener failure: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("listener failure did not reach foreground server")
	}
	if err := runtime.Shutdown(true, time.Second); err != nil {
		t.Fatal(err)
	}
	if active.Err() == nil {
		t.Fatal("server did not cancel the active request")
	}
	listener, err := net.Listen("tcp", live.info.Listen)
	if err != nil {
		t.Fatalf("listener still owns %s: %v", live.info.Listen, err)
	}
	_ = listener.Close()
}
