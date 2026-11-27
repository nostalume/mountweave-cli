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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
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
	saved, err := loadSample(sample.path)
	if err != nil || *saved != *sample {
		t.Fatalf("reopening the local ZIP changed the sample: %v", err)
	}
	sample = saved
	source, err := os.ReadFile(sample.path)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	hold, resume := context.WithCancel(t.Context())
	nativeHold, nativeResume := context.WithCancel(t.Context())
	var nativeRedirects, nativeBodies atomic.Int32
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
		case "/original":
			close(waiting)
			select {
			case <-hold.Done():
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write(source)
		case "/attachment":
			nativeBodies.Add(1)
			w.Header().Set("Content-Disposition", "attachment; filename="+secret+".zip")
			w.Header().Set("Content-Length", fmt.Sprint(len(source)))
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(source[:1])
			w.(http.Flusher).Flush()
			select {
			case <-nativeHold.Done():
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write(source[1:])
		default:
			_, _ = w.Write(source)
		}
	}))
	defer transfer.Close()
	defer resume()
	defer nativeResume()
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download":
			http.Redirect(w, r, transfer.URL+"/original?token="+secret, http.StatusFound)
			return
		case "/native":
			nativeRedirects.Add(1)
			http.Redirect(w, r, transfer.URL+"/attachment?token="+secret, http.StatusFound)
			return
		case "/plain":
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<link rel="icon" href="data:,"><title>fixture document</title>
			<script>window.opener?.postMessage("fixture document", location.origin)</script>`)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<link rel="icon" href="data:,">
		<a id="attachment" href="/native">Download</a>
		<a id="popup" href="/plain?token=%s" target="_blank" rel="opener">Open preview</a>
		<script>
		async function download() {
			for (const path of ['/extra', '/copy', '/decoy', '/large', '/download']) {
				const response = await fetch(path === '/download' ? path : %q + path);
				await response.arrayBuffer();
			}
			await frames();
		}
		function frames() {
			return new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done)));
		}
		</script>`, secret, transfer.URL)
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
	recorder, err := startRecording(ctx, entry, sample, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := func(js string) {
		t.Helper()
		if err := chromedp.Do(ctx, chromedp.Evaluate[chromedp.Void](js, chromedp.EvalAwaitPromise)); err != nil {
			t.Fatal(err)
		}
	}
	run("void (document.title = 'Updated locally')")
	recorder.mark()
	for _, destination := range []string{entry.JoinPath("plain").String(), entry.String()} {
		if err := chromedp.Do(ctx, chromedp.Navigate(destination)); err != nil {
			t.Fatal(err)
		}
	}
	run("frames()")
	recorder.mark()
	run("history.pushState(null, '', '/spa?token=" + secret + "'); frames()")
	run(`(() => {
		const portal = document.createElement("section");
		portal.innerHTML = '<span role="status">重命名成功</span><span>删除成功</span>' +
			'<div role="dialog" hidden><button>普通下载</button><button hidden>打开客户端</button></div>';
		const hidden = document.createElement("span");
		hidden.setAttribute("role", "alert");
		hidden.hidden = true;
		hidden.textContent = "复制成功";
		portal.append(hidden);
		const deleted = document.createElement("span");
		deleted.setAttribute("role", "status");
		deleted.textContent = "文件已删除到回收站";
		document.body.append(portal, deleted);
		portal.querySelector('[role="dialog"]').hidden = false;
		return frames().then(() => { portal.remove(); deleted.remove(); });
	})()`)
	recorder.mark()
	run(`void (window.preview = new Promise(resolve => {
		window.addEventListener("message", event => {
			if (event.origin === location.origin && event.data === "fixture document") {
				event.source.close();
				resolve();
			}
		});
	}))`)
	if err := chromedp.Do(ctx, chromedp.Click(chromedp.ID("popup"))); err != nil {
		t.Fatal(err)
	}
	run("window.preview.then(frames)")
	recorder.mark()
	run("void (window.scenario = download())")
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("the redirected request did not reach the transfer endpoint")
	}
	recorder.mark()
	resume()
	run("window.scenario")
	// Keep the top-level document so the earlier XHR bodies remain cached.
	run(`new Promise((resolve, reject) => {
		const frame = document.createElement("iframe");
		frame.onload = () => frame.contentDocument?.title === "fixture document" ? resolve() : reject("document did not load");
		frame.src = "/plain";
		document.body.append(frame);
	})`)
	if err := chromedp.Do(ctx, chromedp.Click(chromedp.ID("attachment"))); err != nil {
		t.Fatal(err)
	}
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		recorder.mu.Lock()
		verifying := len(recorder.sampleReceipts) != 0
		recorder.mu.Unlock()
		if verifying {
			break
		}
		select {
		case <-poll.C:
		case <-ctx.Done():
			t.Fatal("native attachment verification did not start")
		}
	}

	var reports []networkReport
	var finishErr error
	finishing, finished := make(chan struct{}), make(chan struct{})
	go func() {
		close(finishing)
		reports, finishErr = recorder.finish()
		close(finished)
	}()
	<-finishing
	// The old 300 ms grace elapsed before this attachment could reach EOF.
	select {
	case <-finished:
		t.Fatalf("observation finished while the attachment was still held: %v", finishErr)
	case <-time.After(350 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	nativeResume()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if finishErr != nil {
		t.Fatal(finishErr)
	}
	if len(reports) != 6 || len(reports[0].Requests) != 0 || len(reports[0].Downloads) != 0 ||
		len(reports[0].Navigation) != 0 || !slices.Equal(reports[1].Navigation, []string{"same_tab", "same_tab"}) ||
		!slices.Equal(reports[2].Navigation, []string{"same_document"}) ||
		!slices.Contains(reports[3].Navigation, "window_open_requested") ||
		!slices.Contains(reports[3].Navigation, "new_tab") || reports[3].SampleStreamsOpened != 0 {
		t.Fatalf("unexpected observation phases: %+v", reports)
	}
	tail := reports[5]
	if !slices.Equal(tail.Navigation, []string{"iframe_document"}) {
		t.Fatalf("expected an iframe document and no top-level navigation: %v", tail.Navigation)
	}

	counts := make(map[string]int)
	redirectID, finalID := 0, 0
	for _, response := range reports[4].Requests {
		if response.SampleCheck != "" {
			counts[response.SampleCheck]++
		}
		if response.HTTP == http.StatusFound {
			redirectID = response.Request
			if response.SampleCheck != "" {
				t.Fatal("compared a redirect before its final hop")
			}
		}
		if response.Hop == 2 && response.SampleCheck == "match" {
			finalID = response.Request
		}
	}
	if redirectID == 0 || redirectID != finalID || counts["match"] != 2 || counts["mismatch"] != 1 || counts["unavailable"] != 1 || counts["not_checked"] != 1 {
		t.Fatalf("unexpected sample results after the mark: %v, redirect %d, final %d", counts, redirectID, finalID)
	}
	if !slices.ContainsFunc(tail.Requests, func(response networkObservation) bool {
		return response.HTTP == http.StatusFound && response.Hop == 1 && response.SampleCheck == ""
	}) {
		t.Fatalf("native redirect was missing from the final observation: %+v", tail)
	}
	if !slices.ContainsFunc(tail.Requests, func(response networkObservation) bool {
		return response.Hop == 2 && response.Attachment && response.ContentDisposition == "attachment" &&
			response.BodyCandidate && response.SampleCheck == "match"
	}) || nativeRedirects.Load() != 1 || nativeBodies.Load() != 1 || tail.SampleStreamsOpened != 1 {
		t.Fatalf("native sample mismatch, redirect requests=%d, body requests=%d: %+v", nativeRedirects.Load(), nativeBodies.Load(), tail)
	}
	var notices []string
	for _, phase := range reports {
		notices = append(notices, phase.PageNotices...)
	}
	if len(notices) != 3 || !slices.Contains(notices, "rename_success") || !slices.Contains(notices, "delete_success") ||
		!slices.Contains(notices, "ordinary_download_option") {
		t.Fatalf("expected only the visible notices and download option, got %v", notices)
	}
	t.Logf("Native download events with saving disabled: %v", tail.Downloads)
	raw, err := json.Marshal(reports)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), transfer.URL) || strings.Contains(string(raw), portal.URL) ||
		strings.Contains(string(raw), "重命名成功") || strings.Contains(string(raw), "普通下载") {
		t.Fatal("sample report exposed private data")
	}
	closed := probeWindowClosed(ctx)
	if _, err := chromedp.CallBrowser(ctx, target.CloseTarget, target.CloseTargetParams{
		TargetID: chromedp.FromContext(ctx).Target.TargetID,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("closing the observation tab did not end passive recording")
	}
	file, err := saveProbeReport(filepath.Join(t.TempDir(), "reports"), raw)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatalf("saved report differs from the projected report: %v", err)
	}
}
