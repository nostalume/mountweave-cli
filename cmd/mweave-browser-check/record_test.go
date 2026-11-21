package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
)

func TestObserveOperations(t *testing.T) {
	const prefix = "\nprivate-value\nq denied\nDelete DENIED\n"
	for _, tc := range []struct {
		name, input string
		end         error
	}{
		{"finish", prefix + " \tq \n", nil},
		{"cancel", prefix + "\n", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var prompt bytes.Buffer
			in := bufio.NewReader(strings.NewReader(tc.input))
			captures := 0
			checkSample := tc.end == nil
			observations, err := observeOperations(t.Context(), in, &prompt, checkSample, func(work func() (bool, error)) (networkReport, error) {
				captures++
				keep, err := work()
				if err != nil {
					return networkReport{}, err
				}
				if tc.end == nil && keep != (captures < 3) {
					t.Fatal("only completed observations should be checked")
				}
				reply := replyFacts{HTTP: []int64{200, 206, 204}[captures-1]}
				evidence := networkReport{Status: "observed", Requests: []networkObservation{{replyFacts: reply}}}
				if captures == 3 {
					return evidence, tc.end
				}
				if checkSample && captures == 2 {
					evidence.Requests = append(evidence.Requests,
						networkObservation{Request: 2, SampleCheck: "match"},
						networkObservation{Request: 3, SampleCheck: "unavailable"})
				}
				return evidence, nil
			})
			if !errors.Is(err, tc.end) || len(observations) != 2 || captures != 3 {
				t.Fatalf("wanted two completed observations using three windows, got %+v, %d, %v", observations, captures, err)
			}
			first, second := observations[0], observations[1]
			if first.Operation != "" || first.ReportedResult != "" || first.Requests[0].HTTP != 200 ||
				second.Operation != "delete" || second.ReportedResult != "denied" || second.Requests[0].HTTP != 206 {
				t.Fatalf("actions did not retain their own feedback and network evidence: %+v", observations)
			}
			raw, err := json.Marshal(browserReport{Observations: observations})
			if err != nil || strings.Contains(string(raw), "private-value") || strings.Contains(prompt.String(), "private-value") {
				t.Fatal("report or prompt did not preserve input privacy")
			}
			for _, field := range []string{`"operation":"delete"`, `"reported_result":"denied"`, `"requests":[{`} {
				if !strings.Contains(string(raw), field) {
					t.Fatalf("report is missing %s: %s", field, raw)
				}
			}
			if strings.Count(string(raw), `"operation":`) != 1 {
				t.Fatalf("the unannotated observation should omit the operation: %s", raw)
			}
			summary := prompt.String()
			if checkSample && (!strings.Contains(summary, "Sample check 1. no eligible response observed.") ||
				!strings.Contains(summary, "Sample check 2. matched [2]; body unavailable [3].") ||
				strings.Contains(summary, "Sample check 3.")) {
				t.Fatalf("unexpected sample prompts: %s", summary)
			}
			if !checkSample && strings.Contains(summary, "Sample check") {
				t.Fatal("the regular probe should omit sample checks")
			}
		})
	}
}

func TestCaptureProjection(t *testing.T) {
	const secret = "sensitive-value"
	entry := &url.URL{Scheme: "https", Host: "drive.fixture"}
	eventTime := func(second int64) cdp.MonotonicTime { return cdp.MonotonicTime(second) }
	var requests []requestFacts
	add := func(id network.RequestID, destination string, at int64, kind network.ResourceType, prior *network.Response, rangeHeader bool) {
		t.Helper()
		headers := network.Headers{"Cookie": secret, "Authorization": secret}
		if rangeHeader {
			headers["Range"] = "bytes=3-9"
		}
		projected, ok := projectRequest(requestOrigin(entry), network.EventRequestWillBeSent{
			RequestID: id, Timestamp: eventTime(at), Type: kind,
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
	add("opaque-chromium-id-4", "https://signed.fixture/denied-"+secret, 8, other, nil, false)
	add("opaque-chromium-id-5", "https://drive.fixture/filtered-"+secret, 9, other, nil, false)
	add("opaque-chromium-binary", "https://signed.fixture/binary-"+secret, 12, xhr, nil, false)
	add("opaque-chromium-cut", "https://signed.fixture/cut-"+secret, 13, xhr, nil, false)
	add("opaque-chromium-early", "https://signed.fixture/early-"+secret, 14, other, nil, false)
	finish := func(id network.RequestID, status int64, at int64, kind network.ResourceType, headers network.Headers) responseFacts {
		t.Helper()
		facts, ok := projectResponse(network.EventResponseReceived{
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
		finish("opaque-chromium-id-4", 403, 10, other, nil),
		finish("opaque-chromium-id-5", 204, 11, other, nil),
		finish("opaque-chromium-binary", 200, 15, xhr, network.Headers{
			"Accept-Ranges": "bytes", "Content-Type": "application/octet-stream; confidential=" + secret,
		}),
		finish("opaque-chromium-cut", 200, 17, xhr, network.Headers{"Content-Type": "application/zip"}),
	}
	var transfers []transferFacts
	for _, ev := range []network.EventLoadingFailed{
		{RequestID: "opaque-chromium-early", Timestamp: eventTime(16), Type: other, Canceled: true, ErrorText: secret},
		{RequestID: "opaque-chromium-cut", Timestamp: eventTime(19), Type: xhr, ErrorText: secret},
	} {
		failure, ok := projectFailure(ev)
		if !ok {
			t.Fatal("expected visible transfer failure")
		}
		transfers = append(transfers, failure)
	}
	transfers = append(transfers,
		transferFacts{id: "opaque-chromium-id-1", at: eventTime(7), state: "finished"},
		transferFacts{id: "opaque-chromium-id-2", at: 3.5, state: "finished"},
		transferFacts{id: "opaque-chromium-binary", at: eventTime(14), state: "finished"},
	)
	var recentRequests recentEvents[requestFacts]
	var recentResponses recentEvents[responseFacts]
	// Reproduce a phase that fills before the user acts. Keep the late
	// redirect chain and attachment even after both windows have wrapped.
	for n := range maxPhaseObservations {
		id := network.RequestID("background-" + strconv.Itoa(n))
		recentRequests.add(requestFacts{id: id, method: "GET", resource: other})
		recentResponses.add(finish(id, 204, 0, other, nil))
	}
	for _, request := range requests {
		recentRequests.add(request)
	}
	for _, response := range responses {
		recentResponses.add(response)
	}
	if !recentRequests.truncated || !recentResponses.truncated {
		t.Fatal("the test did not overflow both capture windows")
	}
	report := describeNetwork(recentRequests.ordered(), recentResponses.ordered(), transfers, nil)
	if report.Status != "observed" || len(report.Requests) != 8 || report.UnobservedResponses != 2 {
		t.Fatalf("unexpected phase %+v", report)
	}
	denied := report.Requests[4]
	if denied.HTTP != 403 || denied.Resource != string(other) || denied.Attachment || denied.ContentRange {
		t.Fatalf("ordinary HTTP refusal did not survive projection: %+v", denied)
	}
	first, second, third := report.Requests[0], report.Requests[1], report.Requests[2]
	if first.Hop != 1 || first.HTTP != 302 || first.Relation != "entry-origin" || first.Transfer != "" ||
		second.Hop != 2 || second.Request != first.Request || second.HTTP != 206 || second.Relation != "other-origin" ||
		!second.Attachment || !second.RangeRequested || !second.RangeAdvertised || !second.ContentRange ||
		second.Transfer != "finished" || !second.BodyCandidate ||
		third.Hop != 1 || third.Request == first.Request || third.Route != first.Route || third.Route == second.Route {
		t.Fatalf("redirect or anonymous endpoint binding did not survive projection: %+v", report)
	}
	redirected := report.Requests[3]
	binary, cut, early := report.Requests[5], report.Requests[6], report.Requests[7]
	if redirected.Hop != 2 || redirected.Request != third.Request || redirected.Transfer != "" ||
		redirected.BodyCandidate || redirected.ContentKind != "json" ||
		binary.Transfer != "finished" || !binary.BodyCandidate || binary.Attachment || binary.ContentKind != "binary" ||
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
	report = describeNetwork(recentRequests.ordered(), recentResponses.ordered(), transfers, func(id network.RequestID) string {
		checks++
		if id != "opaque-chromium-binary" {
			t.Fatalf("attempted to read an ineligible response: %s", id)
		}
		return "match"
	})
	if checks != 1 || report.Requests[5].SampleCheck != "match" || report.Requests[6].SampleCheck != "unavailable" {
		t.Fatalf("unexpected whole-file verification: %+v", report)
	}
}
