package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"iter"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const maxPhaseObservations = 64

type browserReport struct {
	Opening      *networkReport    `json:"opening,omitempty"`
	Observations []operationReport `json:"observations"`
}

type operationReport struct {
	Operation      string `json:"operation,omitempty"`
	ReportedResult string `json:"reported_result,omitempty"`
	networkReport
}

type networkReport struct {
	Status              string               `json:"status"`
	Requests            []networkObservation `json:"requests"`
	UnobservedResponses int                  `json:"unobserved_responses,omitempty"`
	Truncated           bool                 `json:"truncated,omitempty"`
}

type networkObservation struct {
	Request        int    `json:"request"`
	Hop            int    `json:"hop"`
	Route          int    `json:"route"`
	Method         string `json:"method"`
	Relation       string `json:"relation"`
	Resource       string `json:"resource"`
	RangeRequested bool   `json:"range_requested"`
	Transfer       string `json:"transfer,omitempty"`
	BodyCandidate  bool   `json:"body_candidate,omitempty"`
	SampleCheck    string `json:"sample_check,omitempty"`
	replyFacts
}

type replyFacts struct {
	HTTP            int64  `json:"http,omitempty"`
	Attachment      bool   `json:"attachment"`
	RangeAdvertised bool   `json:"range_advertised"`
	ContentRange    bool   `json:"content_range_present"`
	ContentKind     string `json:"content_kind,omitempty"`
}

type requestFacts struct {
	id         network.RequestID
	at         cdp.MonotonicTime
	route      [sha256.Size]byte
	method     string
	relation   string
	resource   network.ResourceType
	hasRange   bool
	redirected replyFacts
}

type responseFacts struct {
	id       network.RequestID
	at       cdp.MonotonicTime
	resource network.ResourceType
	replyFacts
}

type transferFacts struct {
	id    network.RequestID
	at    cdp.MonotonicTime
	state string
}

// Keep the most recent events so a long wait before the user's action cannot
// fill the stage with earlier background traffic.
type recentEvents[T any] struct {
	items     [maxPhaseObservations]T
	next      int
	count     int
	truncated bool
}

func (r *recentEvents[T]) add(event T) {
	r.items[r.next] = event
	r.next = (r.next + 1) % len(r.items)
	r.truncated = r.truncated || r.count == len(r.items)
	r.count = min(r.count+1, len(r.items))
}

func (r *recentEvents[T]) ordered() []T {
	if !r.truncated {
		return r.items[:r.count]
	}
	return slices.Concat(r.items[r.next:], r.items[:r.next])
}

func collectEvents[E, T any](events iter.Seq2[E, error], seen *recentEvents[T], project func(E) (T, bool)) {
	for event, err := range events {
		if err != nil {
			return
		}
		if facts, ok := project(event); ok {
			seen.add(facts)
		}
	}
}

// capture observes one action without intercepting the website's requests.
func capture(ctx context.Context, entry *url.URL, sample *fileSample, work func() (bool, error)) (networkReport, error) {
	report := networkReport{Status: "no_evidence", Requests: []networkObservation{}}
	origin := requestOrigin(entry)
	if origin == "" {
		return report, errors.New("the browser entry must have an HTTP origin")
	}
	listen, stop := context.WithCancel(ctx)
	defer stop()
	requests := chromedp.Events(listen, network.RequestWillBeSent)
	responses := chromedp.Events(listen, network.ResponseReceived)
	finished := chromedp.Events(listen, network.LoadingFinished)
	failed := chromedp.Events(listen, network.LoadingFailed)
	zero := int64(0)
	params := network.EnableParams{MaxPostDataSize: &zero}
	if sample != nil {
		single, total := maxSampleResponseBytes, maxSampleResponseBytes*maxSampleResponses
		params.MaxResourceBufferSize, params.MaxTotalBufferSize = &single, &total
	}
	if _, err := chromedp.Call(ctx, network.Enable, params); err != nil {
		return report, errors.Join(errors.New("could not start network observation"), ctx.Err())
	}

	// Keep only the projected facts. Read subscriptions while the action
	// runs so the browser cannot accumulate an unread queue of raw events.
	var seenRequests recentEvents[requestFacts]
	var seenResponses recentEvents[responseFacts]
	var seenFinished, seenFailed recentEvents[transferFacts]
	var observer sync.WaitGroup
	observer.Go(func() {
		collectEvents(requests, &seenRequests, func(ev network.EventRequestWillBeSent) (requestFacts, bool) {
			return projectRequest(origin, ev)
		})
	})
	observer.Go(func() {
		collectEvents(responses, &seenResponses, projectResponse)
	})
	observer.Go(func() {
		collectEvents(finished, &seenFinished, func(ev network.EventLoadingFinished) (transferFacts, bool) {
			return transferFacts{id: ev.RequestID, at: ev.Timestamp, state: "finished"}, true
		})
	})
	observer.Go(func() {
		collectEvents(failed, &seenFailed, projectFailure)
	})

	keep, workErr := work()
	requestsObserved := func(compare func(network.RequestID) string) networkReport {
		stop()
		observer.Wait()
		return describeNetwork(seenRequests.ordered(), seenResponses.ordered(),
			slices.Concat(seenFinished.ordered(), seenFailed.ordered()), compare)
	}
	verify := sample != nil && keep && workErr == nil && ctx.Err() == nil
	if verify {
		// Freeze the action's evidence while the response cache still exists.
		check, cancel := context.WithTimeout(ctx, 2*time.Second)
		report = requestsObserved(func(id network.RequestID) string {
			if check.Err() != nil {
				return "not_checked"
			}
			return sample.compare(check, id)
		})
		cancel()
	}
	cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	_, disableErr := chromedp.Call(cleanup, network.Disable, cdp.Empty{})
	done()
	if !verify {
		report = requestsObserved(nil)
	}
	report.Truncated = seenRequests.truncated || seenResponses.truncated || seenFinished.truncated || seenFailed.truncated
	if disableErr != nil && ctx.Err() == nil {
		disableErr = errors.New("could not stop network observation")
	} else {
		disableErr = nil
	}
	return report, errors.Join(workErr, ctx.Err(), disableErr)
}

func projectRequest(entryOrigin string, ev network.EventRequestWillBeSent) (requestFacts, bool) {
	// An unobserved new destination may still finish the preceding hop.
	request := requestFacts{id: ev.RequestID, at: ev.Timestamp, redirected: projectReply(ev.RedirectResponse)}
	if ev.Request == nil || !observableMethod(ev.Request.Method) || !possibleResource(ev.Type) {
		return request, request.redirected.HTTP != 0
	}
	u, err := url.Parse(ev.Request.URL)
	if err != nil {
		return request, request.redirected.HTTP != 0
	}
	origin := requestOrigin(u)
	if origin == "" {
		return request, request.redirected.HTTP != 0
	}
	relation := "other-origin"
	if origin == entryOrigin {
		relation = "entry-origin"
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	request.route = sha256.Sum256([]byte(origin + path))
	request.method, request.relation, request.resource = ev.Request.Method, relation, ev.Type
	request.hasRange = header(ev.Request.Headers, "Range") != ""
	return request, true
}

func projectResponse(ev network.EventResponseReceived) (responseFacts, bool) {
	facts := projectReply(ev.Response)
	return responseFacts{id: ev.RequestID, at: ev.Timestamp, resource: ev.Type, replyFacts: facts},
		facts.HTTP != 0 && possibleResource(ev.Type)
}

func projectFailure(ev network.EventLoadingFailed) (transferFacts, bool) {
	state := "failed"
	if ev.Canceled {
		state = "canceled"
	}
	return transferFacts{id: ev.RequestID, at: ev.Timestamp, state: state}, possibleResource(ev.Type)
}

func describeNetwork(requests []requestFacts, responses []responseFacts, transfers []transferFacts, compare func(network.RequestID) string) networkReport {
	report := networkReport{Status: "no_evidence", Requests: []networkObservation{}}
	// A redirect reuses Chromium's request ID. Each new request event carries
	// the previous hop's response. Response and transfer events belong to
	// the last hop only if they are current for that request.
	last := make(map[network.RequestID]int, len(requests))
	redirects := make(map[int]replyFacts)
	final := make(map[network.RequestID]responseFacts, len(responses))
	for _, response := range responses {
		final[response.id] = response
	}
	terminal := make(map[network.RequestID]transferFacts, len(transfers))
	for _, transfer := range transfers {
		if old, exists := terminal[transfer.id]; !exists || transfer.at >= old.at {
			terminal[transfer.id] = transfer
		}
	}
	for i, req := range requests {
		if prior, exists := last[req.id]; exists && req.redirected.HTTP != 0 {
			redirects[prior] = req.redirected
		}
		last[req.id] = i
	}
	ids := make(map[network.RequestID]int)
	hops := make(map[network.RequestID]int)
	routes := make(map[[sha256.Size]byte]int)
	type sampleCandidate struct {
		requestID network.RequestID
		index     int
	}
	var candidates []sampleCandidate
	for i, req := range requests {
		ids[req.id] = alias(ids, req.id)
		hops[req.id]++
		if req.method == "" {
			continue
		}
		facts := redirects[i]
		resource := req.resource
		var transfer string
		if i == last[req.id] {
			if response, exists := final[req.id]; exists && response.at >= req.at {
				facts, resource = response.replyFacts, response.resource
			}
			// Chromium can timestamp completion before the header event.
			// The last hop's request start is the correlation boundary.
			if end, exists := terminal[req.id]; exists && end.at >= req.at {
				transfer = end.state
			}
		}
		failed := transfer == "failed" || transfer == "canceled"
		if facts.HTTP == 0 {
			report.UnobservedResponses++
			if !failed {
				continue
			}
		}
		bodyCandidate := req.method == "GET" && (facts.HTTP == http.StatusOK || facts.HTTP == http.StatusPartialContent) &&
			(facts.Attachment || facts.HTTP == http.StatusPartialContent && req.hasRange && facts.ContentRange ||
				transfer == "finished" && facts.ContentKind == "binary")
		var sampleCheck string
		if compare != nil && i == last[req.id] && req.method == "GET" && facts.HTTP == http.StatusOK &&
			!req.hasRange && !facts.ContentRange && (facts.Attachment || facts.RangeAdvertised || facts.ContentKind == "binary") {
			sampleCheck = "unavailable"
			if transfer == "finished" {
				sampleCheck = "not_checked"
				candidates = append(candidates, sampleCandidate{requestID: req.id, index: len(report.Requests)})
			}
		}
		if !observableResource(resource, facts) && !failed && !bodyCandidate && sampleCheck == "" {
			continue
		}
		routes[req.route] = alias(routes, req.route)
		report.Requests = append(report.Requests, networkObservation{
			Request: ids[req.id], Hop: hops[req.id], Route: routes[req.route], Method: req.method,
			Relation: req.relation, Resource: string(resource), RangeRequested: req.hasRange,
			Transfer: transfer, BodyCandidate: bodyCandidate, SampleCheck: sampleCheck, replyFacts: facts,
		})
	}
	// Favor requests observed closest to the end of the action window.
	firstCheck := max(0, len(candidates)-maxSampleResponses)
	for _, candidate := range slices.Backward(candidates[firstCheck:]) {
		report.Requests[candidate.index].SampleCheck = compare(candidate.requestID)
	}
	if len(report.Requests) != 0 {
		report.Status = "observed"
	}
	return report
}

func alias[K comparable](seen map[K]int, key K) int {
	if id, exists := seen[key]; exists {
		return id
	}
	return len(seen) + 1
}

func requestOrigin(u *url.URL) string {
	if u == nil || u.User != nil || u.Hostname() == "" || u.Opaque != "" || u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func observableMethod(method string) bool {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "PROPFIND":
		return true
	default:
		return false
	}
}

func possibleResource(kind network.ResourceType) bool {
	switch kind {
	case "", network.ResourceTypeDocument, network.ResourceTypeXHR, network.ResourceTypeFetch,
		network.ResourceTypeMedia, network.ResourceTypePreflight, network.ResourceTypeOther:
		return true
	default:
		return false
	}
}

func observableResource(kind network.ResourceType, facts replyFacts) bool {
	if kind == network.ResourceTypeOther || kind == "" {
		return facts.Attachment || facts.ContentRange || facts.HTTP == 206 || facts.HTTP >= 400
	}
	return possibleResource(kind)
}

func projectReply(response *network.Response) replyFacts {
	if response == nil || response.Status < 100 || response.Status > 599 {
		return replyFacts{}
	}
	kind, _, _ := strings.Cut(header(response.Headers, "Content-Disposition"), ";")
	declaredType := response.MimeType
	if declaredType == "" {
		declaredType = header(response.Headers, "Content-Type")
	}
	return replyFacts{
		HTTP: response.Status, Attachment: strings.EqualFold(strings.TrimSpace(kind), "attachment"),
		RangeAdvertised: strings.EqualFold(strings.TrimSpace(header(response.Headers, "Accept-Ranges")), "bytes"),
		ContentRange:    header(response.Headers, "Content-Range") != "",
		ContentKind:     contentKind(declaredType),
	}
}

func contentKind(mediaType string) string {
	kind, _, _ := strings.Cut(mediaType, ";")
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch {
	case kind == "application/octet-stream", kind == "binary/octet-stream", kind == "application/pdf",
		kind == "application/zip", kind == "application/gzip", kind == "application/x-7z-compressed", kind == "application/vnd.rar":
		return "binary"
	case kind == "text/html", kind == "application/xhtml+xml":
		return "html"
	case kind == "application/json", strings.HasSuffix(kind, "+json"):
		return "json"
	case kind == "application/xml", kind == "text/xml", strings.HasSuffix(kind, "+xml"):
		return "xml"
	case strings.HasPrefix(kind, "text/"):
		return "text"
	case strings.HasPrefix(kind, "video/"), strings.HasPrefix(kind, "audio/"), strings.HasPrefix(kind, "image/"):
		return "media"
	default:
		return ""
	}
}

func header(headers network.Headers, wanted string) string {
	for name, value := range headers {
		if strings.EqualFold(name, wanted) {
			text, _ := value.(string)
			return text
		}
	}
	return ""
}
