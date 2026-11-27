package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/nostalume/mountweave-cli/internal/website"
)

func TestDiagnosticCapture(t *testing.T) {
	binary, options := website.Browser()
	if binary == "" {
		t.Skip("no compatible browser available")
	}
	requests := make(chan chan struct{})
	pending := make(chan struct{})
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/no-response" {
			close(pending)
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/receipt.js" {
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = io.WriteString(w, `function neverReportThisFunctionName(url, options) { return fetch(url, options); }
				function runReceipt(url, options) { return neverReportThisFunctionName(url, options); }
				function relayReceipt(url, options) { return runReceipt(url, options); }
				function beginReceipt(url, options) { return relayReceipt(url, options); }
				window.fixtureReceipt = (url, options) => beginReceipt(url, options);`)
			return
		}
		if r.URL.Path != "/rename" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<link rel="icon" href="data:,"><title>diagnostic fixture</title><script src="/receipt.js?signature=script-fixture"></script>`)
			return
		}
		release := make(chan struct{})
		select {
		case requests <- release:
		case <-r.Context().Done():
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Set-Cookie", "diagnostic=header-fixture; Path=/")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"fileId":43,"code":0,"refresh_token":"response-fixture"}`)
	}))
	defer portal.Close()
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/outside.js" {
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = io.WriteString(w, `window.outsideReceipt = () => fetch('/outside-caller');`)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", portal.URL)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"fileId":99}`)
	}))
	defer outside.Close()
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
	defer func() {
		if err := closeTab(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(tab, 15*time.Second)
	defer cancel()
	if err := chromedp.Do(ctx, chromedp.Navigate(entry.String())); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []struct {
		name   string
		scope  diagnosticOrigins
		detail bool
	}{
		{name: "anonymous"},
		{name: "scoped", scope: diagnosticOrigins{requestOrigin(entry): true}, detail: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			recorder, err := startRecording(ctx, entry, nil, mode.scope)
			if err != nil {
				t.Fatal(err)
			}
			_, err = chromedp.Run(ctx, chromedp.Evaluate[bool](`(() => {
				window.receipt = fixtureReceipt('/rename?signature=query-fixture', {
					method: 'POST', headers: {'Content-Type': 'application/json', 'Authorization': 'Bearer auth-fixture'},
					body: JSON.stringify({fileId: 43, password: 'request-fixture'})
				});
				return true;
			})()`))
			if err != nil {
				t.Fatal(err)
			}
			select {
			case release := <-requests:
				recorder.mark()
				close(release)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			_, err = chromedp.Run(ctx, chromedp.Evaluate[bool](fmt.Sprintf(`(async () => {
				await (await window.receipt).json();
				await (await fixtureReceipt(%q)).json();
				await new Promise((resolve, reject) => {
					const source = document.createElement('script');
					source.src = %q;
					source.onload = resolve;
					source.onerror = reject;
					document.head.append(source);
				});
				await (await outsideReceipt()).text();
				return true;
			})()`, outside.URL+"/outside", outside.URL+"/outside.js"), chromedp.EvalAwaitPromise))
			if err != nil {
				t.Fatal(err)
			}
			if mode.detail {
				if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`(() => { fetch('/no-response'); return true; })()`)); err != nil {
					t.Fatal(err)
				}
				select {
				case <-pending:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				awaitDiagnosticFixtureCapture(t, ctx, recorder, portal.URL+"/no-response")
				if err := closeTab(); err != nil {
					t.Fatal(err)
				}
			}
			phases, err := recorder.finish()
			if err != nil || len(phases) != 2 {
				t.Fatalf("finish: %d phases, %v", len(phases), err)
			}
			same := findDiagnosticFixtureRequest(t, phases[0], http.StatusCreated)
			other := findDiagnosticFixtureRequest(t, phases[1], http.StatusAccepted)
			if other.Endpoint != "" || len(other.RequestJSON)+len(other.ResponseJSON) != 0 || other.Initiator != nil {
				t.Fatal("capture escaped the selected origin")
			}
			if mode.detail {
				if same.Endpoint != portal.URL+"/rename" ||
					!bytes.Contains(same.RequestJSON, []byte(`"fileId":43`)) ||
					!bytes.Contains(same.ResponseJSON, []byte(`"fileId":43`)) ||
					!bytes.Contains(same.ResponseJSON, []byte(`"code":0`)) {
					t.Fatal("the originating request lost its endpoint or structured receipt")
				}
				if same.Initiator == nil || len(same.Initiator.Frames) != diagnosticFrameLimit || same.Initiator.RequestOffsetMS == nil || *same.Initiator.RequestOffsetMS < 0 {
					t.Fatalf("missing bounded request initiator or timestamp: %+v", same.Initiator)
				}
				for _, frame := range same.Initiator.Frames {
					if frame.Script != portal.URL+"/receipt.js" || frame.Line < 1 || frame.Column < 1 {
						t.Fatalf("unexpected script position: %+v", frame)
					}
				}
				noResponse, unscopedScript := false, false
				for _, request := range phases[1].Requests {
					noResponse = noResponse || request.Endpoint == portal.URL+"/no-response" && request.HTTP == 0
					unscopedScript = unscopedScript || request.Endpoint == portal.URL+"/outside-caller" && request.Initiator == nil
				}
				if !noResponse || !unscopedScript {
					t.Fatal("a scoped request was lost or exposed the unscoped script that initiated it")
				}
			} else if same.Endpoint != "" || len(same.RequestJSON)+len(same.ResponseJSON) != 0 || same.Initiator != nil {
				t.Fatal("anonymous recording exposed details")
			}
			report, err := json.Marshal(phases)
			if err != nil || bytes.Contains(report, []byte("-fixture")) || bytes.Contains(report, []byte("neverReportThisFunctionName")) {
				t.Fatal("report contains fixture credentials or could not be encoded")
			}
		})
	}
}

func awaitDiagnosticFixtureCapture(t *testing.T, ctx context.Context, recorder *recording, pendingEndpoint string) {
	t.Helper()
	ticks := time.NewTicker(10 * time.Millisecond)
	defer ticks.Stop()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		recorder.details.mu.Lock()
		captured := false
		for _, response := range recorder.details.responses {
			captured = captured || response.state == "captured"
		}
		recorder.details.mu.Unlock()
		recorder.mu.Lock()
		tail := recorder.phases[1]
		recordedOther, recordedPending := false, false
		for _, request := range tail.requests.ordered() {
			recordedPending = recordedPending || request.endpoint == pendingEndpoint
		}
		for _, response := range tail.responses.ordered() {
			if response.HTTP == http.StatusAccepted {
				for _, transfer := range tail.transfers.ordered() {
					recordedOther = recordedOther || transfer.id == response.id && transfer.state == "finished"
				}
			}
		}
		recorder.mu.Unlock()
		if captured && recordedOther && recordedPending {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timeout.C:
			t.Fatalf("before browser close, body=%t outside exchange=%t pending request=%t", captured, recordedOther, recordedPending)
		case <-ticks.C:
		}
	}
}

func findDiagnosticFixtureRequest(t *testing.T, phase networkReport, status int64) networkObservation {
	t.Helper()
	for _, req := range phase.Requests {
		if req.HTTP == status && req.Transfer == "finished" {
			return req
		}
	}
	t.Fatalf("the expected HTTP %d exchange did not survive in its phase", status)
	return networkObservation{}
}

func TestDiagnosticMutationEvidence(t *testing.T) {
	if !diagnosticMutationRoute("https://pan.baidu.com/api/filemanager") ||
		!diagnosticMutationRoute("https://up.woozooo.com/doupload.php") ||
		diagnosticMutationRoute("https://example.org/api/filemanager") ||
		diagnosticMutationRoute("https://up.woozooo.com:8443/doupload.php") {
		t.Fatal("provider mutation receipt priority escaped its known origin or lost a route")
	}
	const origin = "https://drive.example"
	const taskID = "9007199254740995"
	form, id := diagnosticForm([]byte("filelist=%5B%7B%22id%22%3A9007199254740993%2C%22password%22%3A%22hidden%22%7D%5D&onnest=fail&token=secret-fixture&taskid=" + taskID))
	if id != taskID || !bytes.Contains(form, []byte("9007199254740993")) ||
		!bytes.Contains(form, []byte(`"onnest":"fail"`)) ||
		bytes.Contains(form, []byte("hidden")) || bytes.Contains(form, []byte("secret-fixture")) ||
		bytes.Contains(form, []byte(taskID)) {
		t.Fatalf("unexpected form projection %s", form)
	}
	if _, id := diagnosticForm([]byte("taskid=&TASK_ID=" + taskID)); id != "" {
		t.Fatal("ambiguous task reference was accepted")
	}
	reports := []networkReport{
		{Requests: []networkObservation{
			{Endpoint: origin + "/api/filemanager", ResponseJSON: json.RawMessage(`{"errno":0,"taskid":` + taskID + `}`)},
		}},
		{Requests: []networkObservation{
			{Endpoint: origin + "/share/taskquery", taskKey: diagnosticTaskKey(origin, taskID), ResponseJSON: json.RawMessage(`{"errno":0,"status":"failed","task_errno":-8}`)},
			{Endpoint: "https://other.example/api/taskquery", taskKey: diagnosticTaskKey("https://other.example", taskID), ResponseJSON: json.RawMessage(`{"errno":0,"status":"success"}`)},
		}},
	}
	linkTasks(reports)
	begin, end, other := reports[0].Requests[0].Task, reports[1].Requests[0].Task, reports[1].Requests[1].Task
	if begin == nil || end == nil || other == nil || begin.Number != end.Number || begin.Number == other.Number ||
		begin.State != "" || end.State != "failed" || end.Errno == nil || *end.Errno != -8 {
		t.Fatalf("unexpected task evidence %+v %+v %+v", begin, end, other)
	}

	nested := []networkReport{{Requests: []networkObservation{
		{Endpoint: origin + "/delete_file", ResponseJSON: json.RawMessage(`{"data":{"taskId":"a"}}`)},
		{Endpoint: origin + "/get_task_status", taskKey: diagnosticTaskKey(origin, "a"), ResponseJSON: json.RawMessage(`{"data":{"status":3}}`)},
		{Endpoint: origin + "/task/get", taskKey: diagnosticTaskKey(origin, "a"), ResponseJSON: json.RawMessage(`{"status":"success","data":{"taskId":"a","taskInfo":{"taskId":"a","status":"Failed"}}}`)},
		{Endpoint: origin + "/task/get", taskKey: diagnosticTaskKey(origin, "a"), ResponseJSON: json.RawMessage(`{"data":{"taskInfo":{"taskId":"another","status":"Succeed"}}}`)},
	}}}
	linkTasks(nested)
	requests := nested[0].Requests
	if requests[0].Task == nil || requests[1].Task == nil || requests[2].Task == nil || requests[3].Task != nil ||
		requests[0].Task.Number != requests[1].Task.Number || requests[0].Task.Number != requests[2].Task.Number ||
		requests[1].Task.State != "3" || requests[2].Task.State != "Failed" {
		t.Fatal("nested receipt lost its task identity or native state")
	}
}
