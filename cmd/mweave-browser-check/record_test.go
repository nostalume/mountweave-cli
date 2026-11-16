package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
)

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
	add("opaque-chromium-id-1", "https://drive.fixture:443/folder-"+secret+"?secret=one", 1, document, nil, false)
	receipt := &network.Response{Status: 302, Headers: network.Headers{"Location": "https://signed.fixture/" + secret}}
	add("opaque-chromium-id-1", "https://signed.fixture/file-"+secret+"?access_token=two", 2, document, receipt, true)
	add("opaque-chromium-id-2", "https://drive.fixture/folder-"+secret+"?secret=two", 3, document, nil, false)
	add("opaque-chromium-id-3", "https://drive.fixture/no-response-"+secret, 7, document, nil, false)
	add("opaque-chromium-id-4", "https://signed.fixture/denied-"+secret, 8, other, nil, false)
	add("opaque-chromium-id-5", "https://drive.fixture/filtered-"+secret, 9, other, nil, false)
	finish := func(status int64, at int64, kind network.ResourceType, headers network.Headers) responseFacts {
		t.Helper()
		facts, ok := projectResponse(network.EventResponseReceived{
			Timestamp: eventTime(at), Type: kind,
			Response: &network.Response{Status: status, Headers: headers},
		})
		if !ok {
			t.Fatalf("capture dropped %s HTTP %d", kind, status)
		}
		return facts
	}
	responses := map[network.RequestID]responseFacts{
		"opaque-chromium-id-2": finish(200, 6, document, nil),
		"opaque-chromium-id-1": finish(206, 5, document, network.Headers{
			"Content-Range": "bytes 3-9/32", "Content-Disposition": "attachment; filename=" + secret,
			"Accept-Ranges": "bytes", "Set-Cookie": secret,
		}),
		"opaque-chromium-id-4": finish(403, 10, other, nil),
		"opaque-chromium-id-5": finish(204, 11, other, nil),
	}
	report := describePhase("download", requests, responses, nil)
	if report.Status != "observed" || len(report.Observations) != 4 || report.UnobservedResponses != 1 {
		t.Fatalf("unexpected phase %+v", report)
	}
	denied := report.Observations[3]
	if denied.HTTP != 403 || denied.Resource != string(other) || denied.Attachment || denied.ContentRange {
		t.Fatalf("ordinary HTTP refusal did not survive projection: %+v", denied)
	}
	first, second, third := report.Observations[0], report.Observations[1], report.Observations[2]
	if first.Hop != 1 || first.HTTP != 302 || first.Relation != "entry-origin" ||
		second.Hop != 2 || second.Request != first.Request || second.HTTP != 206 || second.Relation != "other-origin" ||
		!second.Attachment || !second.RangeRequested || !second.RangeAdvertised || !second.ContentRange ||
		third.Hop != 1 || third.Request == first.Request || third.Route != first.Route || third.Route == second.Route {
		t.Fatalf("redirect or anonymous endpoint binding did not survive projection: %+v", report)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{secret, "drive.fixture", "signed.fixture", "opaque-chromium-id", "bytes 3-9/32", "access_token"} {
		if strings.Contains(string(raw), unsafe) {
			t.Fatalf("report exposed raw network data: %s", raw)
		}
	}
	// If the first omitted request was a redirect, later responses may belong
	// to that omitted hop. They cannot prove an earlier retained request.
	cutoff := eventTime(5)
	bounded := describePhase("download", requests, responses, &cutoff)
	if len(bounded.Observations) != 1 || bounded.Observations[0].HTTP != 302 {
		t.Fatalf("unobserved hops supplied evidence after the capture cutoff: %+v", bounded)
	}
}
