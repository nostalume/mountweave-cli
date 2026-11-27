package website

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

func TestRestoreSessionCookiesWithoutOldTab(t *testing.T) {
	binary := os.Getenv("MWEAVE_TEST_BROWSER")
	if binary == "" {
		t.Skip("set MWEAVE_TEST_BROWSER to run the disposable browser fixture")
	}
	var issueRequests atomic.Int32
	received := make(chan [2]bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/issue":
			issueRequests.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "persistent", Value: "test", Path: "/", HttpOnly: true, MaxAge: 3600})
		case "/check":
			_, temporary := r.Cookie("session")
			_, persistent := r.Cookie("persistent")
			received <- [2]bool{temporary == nil, persistent == nil}
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<title>Disposable browser fixture</title>"))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	profile := t.TempDir()
	options := []chromedp.ExecAllocatorOption{chromedp.Flag("disable-vivaldi", true)}
	first, closeFirst, err := Launch(ctx, binary, options, profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeFirst() })
	if err := chromedp.Do(first, chromedp.Navigate(server.URL+"/issue")); err != nil {
		t.Fatal(err)
	}
	if err := closeFirst(); err != nil {
		t.Fatal(err)
	}

	second, closeSecond, err := LaunchWithSessionCookies(ctx, binary, options, profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeSecond() })
	open, err := chromedp.CallBrowser(second, target.GetTargets, target.GetTargetsParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range open.TargetInfos {
		if page.URL == server.URL+"/issue" {
			t.Fatal("the original tab reopened")
		}
	}
	if err := chromedp.Do(second, chromedp.Navigate(server.URL+"/check")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != [2]bool{true, true} {
			t.Fatalf("session and persistent cookies after reopening: %v", got)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := closeSecond(); err != nil {
		t.Fatal(err)
	}
	if got := issueRequests.Load(); got != 1 {
		t.Fatalf("original tab loaded %d times, want 1", got)
	}
}
