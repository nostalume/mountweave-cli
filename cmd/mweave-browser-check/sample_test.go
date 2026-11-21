package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/nostalume/mountweave-cli/internal/website"
)

func TestSampleComparison(t *testing.T) {
	binary, options := website.Browser()
	if binary == "" {
		t.Skip("no compatible browser available")
	}
	const secret = "private-sample"
	sample, err := createSample(filepath.Join(t.TempDir(), secret+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(sample.path)
	if err != nil {
		t.Fatal(err)
	}
	transfer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/octet-stream")
		switch r.URL.Path {
		case "/decoy":
			_, _ = w.Write(bytes.Repeat([]byte{0x9a}, len(source)))
		case "/large":
			w.Header().Set("Content-Encoding", "gzip")
			compressed := gzip.NewWriter(w)
			_, _ = compressed.Write(bytes.Repeat([]byte{0x9a}, int(maxSampleResponseBytes+1)))
			_ = compressed.Close()
		default:
			_, _ = w.Write(source)
		}
	}))
	defer transfer.Close()
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" {
			http.Redirect(w, r, transfer.URL+"/original?token="+secret, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<script>
		async function download() {
			for (const path of ['/extra', '/download', '/copy', '/decoy', '/large']) {
				const response = await fetch(path === '/download' ? path : %q + path);
				await response.arrayBuffer();
			}
			// Mark after the browser can dispatch the final loadingFinished event.
			await new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done)));
		}
		</script>`, transfer.URL)
	}))
	defer portal.Close()
	entry, err := url.Parse(portal.URL)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancelStartup := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelStartup()
	tab, closeTab, err := website.Launch(startup, binary, options, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeTab(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(tab, 15*time.Second)
	defer cancel()
	if err := chromedp.Do(ctx, chromedp.Navigate(entry.String())); err != nil {
		t.Fatal(err)
	}
	report, err := capture(ctx, entry, sample, func() (bool, error) {
		return true, chromedp.Do(ctx, chromedp.Evaluate[chromedp.Void]("download()", chromedp.EvalAwaitPromise))
	})
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	redirectMatch := false
	for _, response := range report.Requests {
		if response.SampleCheck != "" {
			counts[response.SampleCheck]++
		}
		if response.HTTP == http.StatusFound && response.SampleCheck != "" {
			t.Fatal("compared a redirect before its final hop")
		}
		if response.Hop == 2 && response.SampleCheck == "match" {
			redirectMatch = true
		}
	}
	if !redirectMatch || counts["match"] != 2 || counts["mismatch"] != 1 || counts["unavailable"] != 1 || counts["not_checked"] != 1 {
		t.Fatalf("unexpected sample results: %v", counts)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), transfer.URL) {
		t.Fatal("sample report exposed private data")
	}
}
