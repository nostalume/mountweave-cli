package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const (
	diagnosticBodyLimit  = 64
	diagnosticFrameLimit = 4
)

type scriptPosition struct {
	Script string `json:"script"`
	Line   int64  `json:"line"`
	Column int64  `json:"column"`
}

type requestInitiator struct {
	Frames          []scriptPosition `json:"frames"`
	RequestOffsetMS *int64           `json:"request_offset_ms,omitempty"`
}

type diagnosticOrigins map[string]bool

func parseDiagnosticOrigins(value string, entry *url.URL) (diagnosticOrigins, error) {
	if value == "" {
		return nil, nil
	}
	origins := make(diagnosticOrigins)
	for _, origin := range strings.Split(value, ",") {
		origin = strings.TrimSpace(origin)
		if origin == "site" {
			origins[requestOrigin(entry)] = true
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
			u.Path != "" && u.Path != "/" || requestOrigin(u) == "" {
			return nil, errors.New("Use site or comma-separated HTTPS origins with -details.")
		}
		origins[requestOrigin(u)] = true
	}
	return origins, nil
}

type responseCapture struct {
	at    cdp.MonotonicTime
	body  json.RawMessage
	state string
}

type diagnosticCapture struct {
	origins diagnosticOrigins

	mu        sync.Mutex
	requests  int
	pending   map[observedRequest]responseFacts
	finished  map[observedRequest]float64
	responses map[observedRequest]responseCapture
	readers   chan struct{}
}

func newDiagnosticCapture(origins diagnosticOrigins) *diagnosticCapture {
	return &diagnosticCapture{
		origins: origins, pending: make(map[observedRequest]responseFacts),
		finished: make(map[observedRequest]float64), responses: make(map[observedRequest]responseCapture),
		readers: make(chan struct{}, 4),
	}
}

func (d *diagnosticCapture) endpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !d.origins[requestOrigin(u)] {
		return ""
	}
	return diagnosticEndpoint(raw)
}

func (d *diagnosticCapture) describeRequest(facts *requestFacts, ev network.EventRequestWillBeSent) {
	if ev.Request == nil || facts.method == "" {
		return
	}
	req := ev.Request
	facts.endpoint = d.endpoint(req.URL)
	if facts.endpoint == "" || !diagnosticBodyEndpoint(facts.endpoint) || !structuredResource(ev.Type) {
		return
	}
	facts.initiator = d.scriptPositions(ev.Initiator)
	if u, err := url.Parse(req.URL); err == nil {
		values := u.Query()
		for _, key := range []string{"opera", "type", "action"} {
			if operation := singleDiagnosticValue(values, key); diagnosticOperation(operation) {
				facts.operation = strings.ToLower(operation)
				break
			}
		}
		facts.taskKey = diagnosticTaskKey(facts.endpoint, singleDiagnosticValue(values, "taskid"))
	}
	media, _, _ := strings.Cut(header(req.Headers, "Content-Type"), ";")
	form := strings.EqualFold(strings.TrimSpace(media), "application/x-www-form-urlencoded")
	if !req.HasPostData || !form && contentKind(media) != "json" {
		return
	}
	facts.requestCapture = "unavailable"
	var data []byte
	for i, part := range req.PostDataEntries {
		if part == nil {
			return
		}
		if len(part.Bytes) > maxDiagnosticBodyBytes-len(data) {
			facts.requestCapture = "too_large"
			return
		}
		if i == 0 && len(req.PostDataEntries) == 1 {
			data = part.Bytes
		} else {
			data = append(data, part.Bytes...)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.requests == diagnosticBodyLimit {
		facts.requestCapture = "budget"
		return
	}
	if form {
		body, id := diagnosticForm(data)
		if key := diagnosticTaskKey(facts.endpoint, id); key != ([32]byte{}) {
			facts.taskKey = key
		}
		facts.requestForm = body
	} else {
		facts.requestJSON = diagnosticJSON(data)
		if id, _, _ := diagnosticTaskJSON(facts.requestJSON); id != "" && facts.taskKey == ([32]byte{}) {
			facts.taskKey = diagnosticTaskKey(facts.endpoint, id)
		}
	}
	if facts.requestJSON != nil || facts.requestForm != nil {
		d.requests++
		facts.requestCapture = "captured"
	}
}

func (d *diagnosticCapture) scriptPositions(initiator *network.Initiator) []scriptPosition {
	if initiator == nil || initiator.Type != network.InitiatorTypeScript || initiator.Stack == nil {
		return nil
	}
	var positions []scriptPosition
	frames := initiator.Stack.CallFrames
	for _, frame := range frames[:min(len(frames), 12)] {
		if frame == nil || frame.LineNumber < 0 || frame.ColumnNumber < 0 {
			continue
		}
		script := d.endpoint(frame.URL)
		if script == "" || len(script) > 512 || !diagnosticBodyEndpoint(script) {
			continue
		}
		positions = append(positions, scriptPosition{Script: script, Line: frame.LineNumber + 1, Column: frame.ColumnNumber + 1})
		if len(positions) == diagnosticFrameLimit {
			break
		}
	}
	return positions
}

func (d *diagnosticCapture) response(r *recording, ctx context.Context, facts *responseFacts, ev network.EventResponseReceived) {
	if !structuredResource(ev.Type) || facts.Attachment ||
		facts.HTTP >= 300 && facts.HTTP < 400 || ev.Response == nil {
		return
	}
	endpoint := d.endpoint(ev.Response.URL)
	if endpoint == "" || !diagnosticBodyEndpoint(endpoint) {
		return
	}
	priority := diagnosticMutationRoute(endpoint)
	// Some mutation endpoints send structured receipts as text/plain.
	if facts.ContentKind != "json" && !(priority && facts.ContentKind == "text") {
		return
	}
	facts.capture = true
	facts.priority = priority
	d.mu.Lock()
	defer d.mu.Unlock()
	if size, ok := d.finished[facts.id]; ok {
		delete(d.finished, facts.id)
		d.read(r, ctx, *facts, size)
	} else if len(d.pending) < diagnosticBodyLimit {
		d.pending[facts.id] = *facts
	}
}

func (d *diagnosticCapture) complete(r *recording, ctx context.Context, id observedRequest, size float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if facts, ok := d.pending[id]; ok {
		delete(d.pending, id)
		d.read(r, ctx, facts, size)
	} else {
		if len(d.finished) >= 2*diagnosticBodyLimit {
			for evicted := range d.finished {
				delete(d.finished, evicted)
				break
			}
		}
		d.finished[id] = size
	}
}

func (d *diagnosticCapture) failed(id observedRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.pending, id)
	delete(d.finished, id)
}

// read pairs a JSON response with its terminal event under d.mu. Any returned
// body belongs to this target and response timestamp.
func (d *diagnosticCapture) read(r *recording, ctx context.Context, facts responseFacts, encodedSize float64) {
	limit := diagnosticBodyLimit
	if !facts.priority {
		limit -= 16 // Leave room for mutations and task receipts.
	}
	if len(d.responses) >= limit {
		return
	}
	if _, exists := d.responses[facts.id]; exists {
		return
	}
	outcome := responseCapture{at: facts.at, state: "unavailable"}
	if encodedSize > maxDiagnosticBodyBytes+4096 {
		outcome.state = "too_large"
	}
	d.responses[facts.id] = outcome
	if outcome.state == "too_large" {
		return
	}
	r.listeners.Go(func() {
		ctx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
		defer cancel()
		select {
		case <-ctx.Done():
			return
		case d.readers <- struct{}{}:
		}
		defer func() { <-d.readers }()
		reply, err := chromedp.Call(ctx, network.GetResponseBody, network.GetResponseBodyParams{RequestID: facts.id.id})
		if err != nil {
			return
		}
		if len(reply.Body) > maxDiagnosticBodyBytes {
			outcome.state = "too_large"
		} else if outcome.body = diagnosticJSON(reply.Body); outcome.body != nil {
			outcome.state = "captured"
		}
		d.mu.Lock()
		d.responses[facts.id] = outcome
		d.mu.Unlock()
	})
}

func structuredResource(resource network.ResourceType) bool {
	return resource == network.ResourceTypeFetch || resource == network.ResourceTypeXHR
}

func diagnosticBodyEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		key := diagnosticKey(segment)
		if diagnosticCredentialKey(key) {
			return false
		}
		switch key {
		case "authorize", "oauth", "oauth2", "login", "signin", "logout", "sso",
			"qrcode", "captcha":
			return false
		}
	}
	return true
}
