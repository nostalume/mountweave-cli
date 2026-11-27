package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/nostalume/mountweave-cli/internal/website"
)

func TestObserveOperations(t *testing.T) {
	const prefix = "2\n\nprivate-value\nDelete DENIED\n"
	for _, tc := range []struct {
		name, input    string
		marks          int
		first, visible string
		existingSample bool
		end            error
	}{
		{"finish", prefix + "\n \tq \n", 3, "upload", "", false, nil},
		{"visible outcome", prefix + "2\nq\n", 3, "upload", "client", false, nil},
		{"existing file", "7\n5\n", 1, "download", "no_visible_change", false, nil},
		{"existing sample", "2\n1\nq\n", 2, "download", "client", true, nil},
		{"quit before download mark", "\nq\n", 1, "upload", "", false, nil},
		{"interrupted", prefix, 2, "", "", false, io.EOF},
		{"no marks", "q\n", 0, "", "", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var prompt bytes.Buffer
			in := bufio.NewReader(strings.NewReader(tc.input))
			marks := 0
			sample := tc.first == "upload" || tc.existingSample
			observations, err := observeOperations(t.Context(), in, &prompt, tc.first, func() { marks++ })
			if !errors.Is(err, tc.end) || marks != tc.marks || len(observations) != marks {
				t.Fatalf("observations=%+v, marks=%d, err=%v", observations, marks, err)
			}
			phases := make([]networkReport, marks+2)
			phases[len(phases)-1].Navigation = []string{"window_open_requested"}
			attachment := networkObservation{Method: "GET", replyFacts: replyFacts{HTTP: 200, Attachment: true}}
			if tc.existingSample {
				phases[1].UnlinkedSampleChecks, phases[1].Truncated = []string{"match"}, true
				phases[1].SampleStreamsOpened = 1
				attachment.SampleCheck = "unavailable"
				phases[1].Requests = []networkObservation{attachment, attachment}
			} else if tc.first == "download" {
				phases[1].Requests = []networkObservation{{Method: "GET", BodyCandidate: true, Transfer: "finished", replyFacts: replyFacts{HTTP: 200}}}
			} else if tc.first == "upload" && marks == 1 {
				attachment.SampleCheck = "match"
				phases[1].Requests = []networkObservation{attachment}
			}
			if marks >= 2 {
				phases[2].Downloads = []string{"canceled"}
				phases[2].Navigation = []string{"new_tab"}
				phases[2].Requests = []networkObservation{attachment}
			}
			if marks == 3 {
				phases[3].Requests = []networkObservation{{Request: 2, SampleCheck: "match"}}
			}
			report := browserReport{Observations: observations, DownloadSaving: "blocked"}
			if err := report.complete(&prompt, phases, sample); err != nil {
				t.Fatal(err)
			}
			if len(report.Observations) != marks+1 {
				t.Fatal("unmarked evidence was lost")
			}
			tail := report.Observations[marks]
			if tail.Operation != "" || tail.ReportedResult != "" || !slices.Equal(tail.Navigation, []string{"window_open_requested"}) {
				t.Fatalf("unmarked navigation acquired an intent or outcome: %+v", tail)
			}
			raw, err := json.Marshal(report)
			if err != nil || strings.Contains(string(raw), "private-value") || strings.Contains(prompt.String(), "private-value") {
				t.Fatal("report or prompt did not preserve input privacy")
			}
			summary := prompt.String()
			if tc.first == "upload" && marks == 1 && !strings.Contains(summary, "full sample received and matched") {
				t.Fatalf("early sample verdict was lost: %s", summary)
			}
			if !strings.Contains(summary, "browser page activity (window open requested)") {
				t.Fatalf("unmarked window request was lost: %s", summary)
			}
			if marks == 0 && !strings.Contains(summary, "No action was marked.") {
				t.Fatal(summary)
			}
			if tc.first == "download" && (report.Observations[0].Operation != "download" ||
				report.Observations[0].ReportedResult != tc.visible || strings.Contains(summary, "Sample check") != tc.existingSample ||
				!strings.Contains(summary, "Press Enter to mark this route, or q then Enter to finish.")) {
				t.Fatalf("focused check lost the first route: %s\n%s", raw, summary)
			}
			if tc.existingSample && (!strings.Contains(summary, "Network evidence was truncated.") ||
				!strings.Contains(string(raw), `"operation":"download","reported_result":"preview"`) ||
				!strings.Contains(summary, "full sample received and matched [request link unavailable]") ||
				!strings.Contains(summary, "HTTP attachment headers observed (2), no linked byte comparison, attachment streams opened by the probe (1)") ||
				strings.Contains(summary, "no browser download start observed")) {
				t.Fatalf("existing sample verdict was obscured: %s\n%s", raw, summary)
			}
			if tc.first == "download" && !tc.existingSample && (!strings.Contains(summary, "HTTP body candidates without attachment headers (1)") ||
				strings.Contains(summary, "no browser download start observed")) {
				t.Fatalf("body candidate without a native download was obscured: %s\n%s", raw, summary)
			}
			if marks >= 2 && tc.first != "download" && (!strings.Contains(string(raw), `"operation":"delete","reported_result":"denied"`) ||
				!strings.Contains(summary, "HTTP attachment headers observed (1), no linked byte comparison, browser download event (canceled, local saving blocked), browser page activity (new tab)")) {
				t.Fatalf("lost evidence: %s\n%s", raw, summary)
			}
			if marks == 3 && (report.Observations[0].Operation != "upload" || report.Observations[2].Operation != "download" ||
				report.Observations[2].ReportedResult != tc.visible || strings.Contains(summary, "no browser download start observed") ||
				!strings.Contains(summary, "Sample check 3.")) {
				t.Fatalf("sample workflow lost its steps: %s\n%s", raw, summary)
			}
		})
	}
}

func TestCaptureProjection(t *testing.T) {
	const secret = "sensitive-value"
	frame := &cdp.Frame{ParentID: "fixture-page", URL: "https://fixture.example/" + secret}
	if kind, ok := frameNavigation(page.EventFrameNavigated{Frame: frame}); !ok || kind != "iframe_document" {
		t.Fatalf("expected anonymous iframe navigation, got %q", kind)
	}
	if externalProtocol(frame.URL) || externalProtocol("blob:"+frame.URL) || !externalProtocol("fixture-app:"+secret) {
		t.Fatal("external protocol attempt was not distinguished from a web document")
	}
	entry := &url.URL{Scheme: "https", Host: "drive.fixture"}
	eventTime := func(second int64) cdp.MonotonicTime { return cdp.MonotonicTime(second) }
	var requests []requestFacts
	add := func(id network.RequestID, destination string, at int64, kind network.ResourceType, prior *network.Response, rangeHeader bool) {
		t.Helper()
		headers := network.Headers{"Cookie": secret, "Authorization": secret}
		if rangeHeader {
			headers["Range"] = "bytes=3-9"
		}
		projected, ok := projectRequest(requestOrigin(entry), 0, network.EventRequestWillBeSent{
			RequestID: id, Timestamp: eventTime(at), Type: kind, FrameID: "fixture-frame",
			Request: &network.Request{URL: destination, Method: "GET", Headers: headers}, RedirectResponse: prior,
		})
		if !ok {
			t.Fatalf("missing projected request for %q", id)
		}
		requests = append(requests, projected)
	}
	const document = network.ResourceTypeDocument
	const other = network.ResourceTypeOther
	const xhr = network.ResourceTypeXHR
	add("opaque-chromium-id-1", "https://drive.fixture:443/folder-"+secret+"?secret=one", 1, document, nil, false)
	receipt := &network.Response{Status: 302, Headers: network.Headers{"Location": "https://signed.fixture/" + secret}}
	add("opaque-chromium-id-1", "https://signed.fixture/file-"+secret+"?access_token=two", 2, document, receipt, true)
	add("opaque-chromium-id-2", "https://drive.fixture/folder-"+secret+"?secret=two", 3, document, nil, false)
	add("opaque-chromium-id-2", "https://drive.fixture/next-"+secret, 4, document, receipt, false)
	add("opaque-chromium-id-3", "https://drive.fixture/no-response-"+secret, 7, document, nil, false)
	ambiguous := "https://signed.fixture/repeated-" + secret
	add("opaque-chromium-id-4", ambiguous, 8, other, nil, false)
	add("opaque-chromium-id-5", ambiguous, 9, other, nil, false)
	add("opaque-chromium-binary", "https://signed.fixture/binary-"+secret, 12, xhr, nil, false)
	add("opaque-chromium-cut", "https://signed.fixture/cut-"+secret, 13, xhr, nil, false)
	add("opaque-chromium-early", "https://signed.fixture/early-"+secret, 14, other, nil, false)
	finish := func(id network.RequestID, status int64, at int64, kind network.ResourceType, headers network.Headers) responseFacts {
		t.Helper()
		facts, ok := projectResponse(0, network.EventResponseReceived{
			RequestID: id, Timestamp: eventTime(at), Type: kind,
			Response: &network.Response{Status: status, Headers: headers},
		})
		if !ok {
			t.Fatalf("capture dropped %s HTTP %d", kind, status)
		}
		return facts
	}
	responses := []responseFacts{
		finish("opaque-chromium-id-1", 206, 5, document, network.Headers{
			"Content-Range": "bytes 3-9/32", "Content-Disposition": "attachment; filename=" + secret,
			"Accept-Ranges": "bytes", "Set-Cookie": secret,
		}),
		finish("opaque-chromium-id-2", 200, 6, document, network.Headers{
			"Accept-Ranges": "bytes", "Content-Type": "application/json; confidential=" + secret,
		}),
		finish("opaque-chromium-id-4", 403, 10, other, network.Headers{"Content-Disposition": "private-" + secret}),
		finish("opaque-chromium-id-5", 204, 11, other, nil),
		finish("opaque-chromium-binary", 200, 15, xhr, network.Headers{
			"Accept-Ranges": "bytes", "Content-Type": "application/octet-stream; confidential=" + secret,
			"Content-Disposition": "inline; filename=" + secret,
		}),
		finish("opaque-chromium-cut", 200, 17, xhr, network.Headers{"Content-Type": "application/zip"}),
	}
	var transfers []transferFacts
	for _, ev := range []network.EventLoadingFailed{
		{RequestID: "opaque-chromium-early", Timestamp: eventTime(16), Type: other, Canceled: true, ErrorText: secret},
		{RequestID: "opaque-chromium-cut", Timestamp: eventTime(19), Type: xhr, ErrorText: secret},
	} {
		failure, ok := projectFailure(0, ev)
		if !ok {
			t.Fatal("expected visible transfer failure")
		}
		transfers = append(transfers, failure)
	}
	transfers = append(transfers,
		transferFacts{id: observedRequest{id: "opaque-chromium-id-1"}, at: eventTime(7), state: "finished"},
		transferFacts{id: observedRequest{id: "opaque-chromium-id-2"}, at: 3.5, state: "finished"},
		transferFacts{id: observedRequest{id: "opaque-chromium-binary"}, at: eventTime(14), state: "finished"},
	)
	phase, tail := &recordedPhase{}, &recordedPhase{}
	// Reproduce a phase that fills before the user acts. Keep the late
	// redirect chain and attachment even after both windows have wrapped.
	for n := range maxPhaseObservations {
		id := network.RequestID("background-" + strconv.Itoa(n))
		phase.requests.add(requestFacts{id: observedRequest{id: id}, method: "GET", resource: other})
		tail.responses.add(finish(id, 204, 0, other, nil))
	}
	for _, request := range requests {
		phase.requests.add(request)
	}
	for _, response := range responses {
		tail.responses.add(response)
	}
	for _, transfer := range transfers {
		tail.transfers.add(transfer)
	}
	phase.downloads.add(downloadEvent{guid: secret, state: "canceled"})
	tail.downloads.add(downloadEvent{guid: secret, state: "started", target: requests[1].download})
	tail.downloads.add(downloadEvent{guid: "anonymous", state: "started", target: newDownloadTarget("fixture-frame", ambiguous)})
	phases := []*recordedPhase{phase, tail}
	reports := describeRecording(phases, nil, nil)
	report := reports[0]
	if report.Status != "observed" || len(report.Requests) != 8 || report.UnobservedResponses != 2 || !report.Truncated {
		t.Fatalf("unexpected phase %+v", report)
	}
	if !slices.Equal(report.Downloads, []string{"canceled"}) || !slices.Equal(reports[1].Downloads, []string{"started"}) ||
		len(reports[1].Requests) != 0 || !reports[1].Truncated {
		t.Fatalf("late or ambiguous downloads assigned to the wrong phase: %+v", reports)
	}
	denied := report.Requests[4]
	if denied.HTTP != 403 || denied.Resource != string(other) || denied.Attachment || denied.ContentRange || denied.ContentDisposition != "other" {
		t.Fatalf("ordinary HTTP refusal did not survive projection: %+v", denied)
	}
	first, second, third := report.Requests[0], report.Requests[1], report.Requests[2]
	if first.Hop != 1 || first.HTTP != 302 || first.Relation != "entry-origin" || first.Transfer != "" || first.ContentDisposition != "" ||
		second.Hop != 2 || second.Request != first.Request || second.HTTP != 206 || second.Relation != "other-origin" ||
		!second.Attachment || second.ContentDisposition != "attachment" || !second.RangeRequested || !second.RangeAdvertised || !second.ContentRange ||
		second.Transfer != "finished" || !second.BodyCandidate ||
		third.Hop != 1 || third.Request == first.Request || third.Route != first.Route || third.Route == second.Route {
		t.Fatalf("redirect or anonymous endpoint binding did not survive projection: %+v", report)
	}
	redirected := report.Requests[3]
	binary, cut, early := report.Requests[5], report.Requests[6], report.Requests[7]
	if redirected.Hop != 2 || redirected.Request != third.Request || redirected.Transfer != "" ||
		redirected.BodyCandidate || redirected.ContentKind != "json" ||
		binary.Transfer != "finished" || !binary.BodyCandidate || binary.Attachment || binary.ContentKind != "binary" || binary.ContentDisposition != "inline" ||
		cut.Transfer != "failed" || cut.BodyCandidate || cut.HTTP != 200 || cut.ContentKind != "binary" ||
		early.Transfer != "canceled" || early.HTTP != 0 || early.BodyCandidate {
		t.Fatalf("transport states or body candidates did not survive projection: %+v", report)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{secret, "drive.fixture", "signed.fixture", "opaque-chromium-", "bytes 3-9/32", "access_token", `"http":0`} {
		if strings.Contains(string(raw), unsafe) {
			t.Fatalf("report exposed raw network data: %s", raw)
		}
	}
	if strings.Contains(string(raw), "sample_check") {
		t.Fatal("default observation should not compare samples")
	}
	checks := 0
	reports = describeRecording(phases, func(id network.RequestID) string {
		checks++
		if id != "opaque-chromium-binary" {
			t.Fatalf("attempted to read an ineligible response: %s", id)
		}
		return "match"
	}, map[observedRequest]sampleReceipt{{id: "evicted-request"}: {check: "match", phase: 1, streamOpened: true}})
	report = reports[0]
	if checks != 1 || report.Requests[5].SampleCheck != "match" || report.Requests[6].SampleCheck != "unavailable" {
		t.Fatalf("unexpected whole-file verification: %+v", report)
	}
	if !slices.Equal(reports[1].UnlinkedSampleChecks, []string{"match"}) || len(reports[1].Requests) != 0 ||
		reports[1].SampleStreamsOpened != 1 {
		t.Fatalf("a validated sample lost its evidence after request eviction: %+v", reports[1])
	}
}

func TestRelatedTabNetwork(t *testing.T) {
	binary, options := website.Browser()
	if binary == "" {
		t.Skip("no supported browser")
	}
	const filename = "fixture-secret-附件.zip"
	content := []byte("related tab fixture attachment")
	unrelated := make(chan struct{}, 1)
	transferClosed := make(chan time.Time, 1)
	serveAttachment := func(w http.ResponseWriter, data []byte) {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=\"fixture-secret-fallback.zip\"; filename*=UTF-8''"+url.PathEscape(filename))
		// Sending headers first forces an unknown length on the wire.
		_ = http.NewResponseController(w).Flush()
		_, _ = w.Write(data)
	}
	transfer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serveAttachment(w, content)
	}))
	defer transfer.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<button id="open" onclick="window.open('/landing')">Open</button>
			<button id="hold" onclick="window.open('/child?hold=1')">Hold</button>`)
	})
	mux.HandleFunc("/landing", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<a id="download" href="/transfer">Download</a>`)
	})
	mux.HandleFunc("/transfer", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, transfer.URL+"/original?ticket=fixture-secret", http.StatusFound)
	})
	mux.HandleFunc("/child", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/attachment?private=fixture-secret&hold="+req.URL.Query().Get("hold"), http.StatusFound)
	})
	mux.HandleFunc("/attachment", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("hold") == "1" {
			serveAttachment(w, content[:1])
			_ = http.NewResponseController(w).Flush()
			<-req.Context().Done()
			select {
			case transferClosed <- time.Now():
			default:
			}
			return
		}
		serveAttachment(w, content)
	})
	mux.HandleFunc("/unrelated", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<script>location.replace('/unrelated-attachment')</script>")
	})
	mux.HandleFunc("/unrelated-attachment", func(w http.ResponseWriter, _ *http.Request) {
		serveAttachment(w, content)
		_ = http.NewResponseController(w).Flush()
		select {
		case unrelated <- struct{}{}:
		default:
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	entry, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// Wait for an actual CDP stream read before canceling the held response.
	readStarted := make(chan struct{}, 1)
	tab, closeTab, err := website.Launch(ctx, binary, options, t.TempDir(), chromedp.WithDebugf(func(format string, args ...any) {
		if format != "-> %s" || len(args) != 1 {
			return
		}
		if message, ok := args[0].([]byte); ok && bytes.Contains(message, []byte(`"method":"IO.read"`)) {
			select {
			case readStarted <- struct{}{}:
			default:
			}
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTab()
	life, stop := context.WithCancel(tab)
	defer stop()
	r, err := startRecording(life, entry, sampleFromBytes(filename, content), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		r.stop()
		r.listeners.Wait()
	}()
	actions, endActions := context.WithTimeout(life, 12*time.Second)
	defer endActions()
	if err := chromedp.Do(actions, chromedp.Navigate(entry.String())); err != nil {
		t.Fatal(err)
	}
	r.mark()
	if _, err := chromedp.CallBrowser(actions, target.CreateTarget, target.CreateTargetParams{
		URL: entry.JoinPath("unrelated").String(), Background: new(true),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-unrelated:
	case <-actions.Done():
		t.Fatal("the independent tab never requested the sample")
	}
	r.mark()
	parentID := chromedp.FromContext(tab).Target.TargetID
	targets := chromedp.BrowserEvents(actions, target.TargetCreated)
	if err := chromedp.Do(actions, chromedp.Click(chromedp.CSS("#open"))); err != nil {
		t.Fatal(err)
	}
	var popup target.ID
	for event, err := range targets {
		if err != nil {
			t.Fatal(err)
		}
		if info := event.TargetInfo; info != nil && info.Type == "page" && info.OpenerID == parentID {
			popup = info.TargetID
			break
		}
	}
	if popup == "" {
		t.Fatal("the download page did not open")
	}
	child, detachChild := chromedp.NewContext(actions, chromedp.WithTargetID(popup), chromedp.WithDetachOnCancel())
	defer detachChild()
	if err := chromedp.Do(child, chromedp.WaitVisible(chromedp.CSS("#download"))); err != nil {
		t.Fatal(err)
	}
	r.mark()
	if err := chromedp.Do(child, chromedp.Click(chromedp.CSS("#download"))); err != nil {
		t.Fatal(err)
	}
	awaitRead := func() {
		t.Helper()
		select {
		case <-readStarted:
		case <-actions.Done():
			t.Fatal("the related tab never began reading its attachment stream")
		}
	}
	awaitRead()
	r.sampleReads.Wait()
	detachChild()
	// A second read to verify EOF can leave a signal from the first download.
	select {
	case <-readStarted:
	default:
	}
	r.mark()
	if _, err := chromedp.Call(actions, page.BringToFront, cdp.Empty{}); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Do(actions, chromedp.Click(chromedp.CSS("#hold"))); err != nil {
		t.Fatal(err)
	}
	awaitRead()
	stoppingAt := time.Now()
	stop()
	reports, err := r.finish()
	finishedIn := time.Since(stoppingAt)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case releasedAt := <-transferClosed:
		t.Logf("related transfer canceled in %s, recorder finished in %s", releasedAt.Sub(stoppingAt), finishedIn)
	case <-ctx.Done():
		t.Fatal("the server still has the canceled related transfer")
	}
	probe, endProbe := context.WithTimeout(tab, 2*time.Second)
	defer endProbe()
	remaining, err := chromedp.CallBrowser(probe, target.GetTargets, target.GetTargetsParams{})
	if err != nil {
		t.Fatal(err)
	}
	parent := chromedp.FromContext(tab).Target.TargetID
	if !slices.ContainsFunc(remaining.TargetInfos, func(info *target.Info) bool { return info.TargetID == parent && info.Attached }) {
		t.Fatal("canceling the observation closed or detached the original page")
	}
	if slices.ContainsFunc(remaining.TargetInfos, func(info *target.Info) bool {
		return info.OpenerID == parent && info.Attached
	}) {
		t.Fatal("a related target still has an attached session after cancellation")
	}
	if len(reports) != 5 || slices.Contains(reports[1].Navigation, "new_tab") || reports[1].SampleStreamsOpened != 0 ||
		len(reports[1].UnlinkedSampleChecks) != 0 || slices.ContainsFunc(reports[1].Requests, func(req networkObservation) bool {
		return req.RelatedTab != 0 || req.Attachment || req.SampleCheck != ""
	}) {
		t.Fatalf("independent tab leaked into the recording: %+v", reports)
	}
	landing := reports[2]
	if !slices.Contains(landing.Navigation, "new_tab_attached") || landing.SampleStreamsOpened != 0 ||
		!slices.ContainsFunc(landing.Requests, func(req networkObservation) bool {
			return req.RelatedTab == 1 && req.HTTP == http.StatusOK && req.ContentKind == "html" && !req.Attachment
		}) {
		t.Fatalf("related HTML landing page not distinguished from the attachment: %+v", landing)
	}
	report := reports[3]
	if report.SampleStreamsOpened != 1 || len(report.UnlinkedSampleChecks) != 0 {
		t.Fatalf("expected one linked sample from the related tab: %+v", report)
	}
	if !slices.ContainsFunc(report.Requests, func(req networkObservation) bool {
		return req.RelatedTab == 1 && req.Hop == 2 && req.Relation == "other-origin" && req.HTTP == http.StatusOK &&
			req.Attachment && req.SampleCheck == "match"
	}) {
		t.Fatalf("related tab attachment and redirect missing: %+v", report)
	}
	canceled := reports[4]
	if canceled.SampleStreamsOpened != 1 || len(canceled.UnlinkedSampleChecks) != 0 || !slices.ContainsFunc(canceled.Requests, func(req networkObservation) bool {
		return req.RelatedTab == 2 && req.Hop == 2 && req.SampleCheck == "read_incomplete"
	}) || slices.ContainsFunc(canceled.Requests, func(req networkObservation) bool { return req.SampleCheck == "match" }) {
		t.Fatalf("canceled related attachment was not recorded without a match: %+v", canceled)
	}
	raw, err := json.Marshal(reports)
	if err != nil || bytes.Contains(raw, []byte("fixture-secret")) || bytes.Contains(raw, []byte(server.URL)) || bytes.Contains(raw, content) {
		t.Fatalf("related tab report exposed raw network data or failed to encode: %v", err)
	}
}
