package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

const maxPhaseObservations = 64

type phaseReport struct {
	Stage               string               `json:"stage"`
	Status              string               `json:"status"`
	Observations        []networkObservation `json:"observations"`
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
	replyFacts
}

type replyFacts struct {
	HTTP            int64 `json:"http"`
	Attachment      bool  `json:"attachment"`
	RangeAdvertised bool  `json:"range_advertised"`
	ContentRange    bool  `json:"content_range_present"`
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
	at       cdp.MonotonicTime
	resource network.ResourceType
	replyFacts
}

// capture projects network metadata during one action window. It leaves the
// website's requests untouched and retains no body contents from browser events.
func capture(ctx context.Context, stage string, entry *url.URL, work func() error) (phaseReport, error) {
	report := phaseReport{Stage: stage, Status: "no_evidence", Observations: []networkObservation{}}
	origin := requestOrigin(entry)
	if origin == "" {
		return report, errors.New("the browser entry must have an HTTP origin")
	}
	listen, stop := context.WithCancel(ctx)
	defer stop()
	requests := chromedp.Events(listen, network.RequestWillBeSent)
	responses := chromedp.Events(listen, network.ResponseReceived)
	zero := int64(0)
	if _, err := chromedp.Call(ctx, network.Enable, network.EnableParams{MaxPostDataSize: &zero}); err != nil {
		return report, errors.Join(errors.New("could not start network observation"), ctx.Err())
	}

	// Keep only the projected facts. Read both subscriptions while the action
	// runs so the browser cannot accumulate an unread queue of raw events.
	var seenRequests []requestFacts
	seenResponses := make(map[network.RequestID]responseFacts)
	var requestCutoff *cdp.MonotonicTime
	var responsesCut bool
	var observer sync.WaitGroup
	observer.Go(func() {
		for ev, err := range requests {
			if err != nil {
				return
			}
			request, ok := projectRequest(origin, ev)
			if !ok {
				continue
			}
			if len(seenRequests) == maxPhaseObservations {
				if requestCutoff == nil {
					cut := request.at
					requestCutoff = &cut
				}
				continue
			}
			seenRequests = append(seenRequests, request)
		}
	})
	observer.Go(func() {
		for ev, err := range responses {
			if err != nil {
				return
			}
			response, ok := projectResponse(ev)
			if !ok {
				continue
			}
			if _, found := seenResponses[ev.RequestID]; !found && len(seenResponses) == maxPhaseObservations {
				responsesCut = true
				continue
			}
			seenResponses[ev.RequestID] = response
		}
	})

	workErr := work()
	cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	_, disableErr := chromedp.Call(cleanup, network.Disable, cdp.Empty{})
	done()
	stop()
	observer.Wait()

	report = describePhase(stage, seenRequests, seenResponses, requestCutoff)
	report.Truncated = requestCutoff != nil || responsesCut
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
	return responseFacts{at: ev.Timestamp, resource: ev.Type, replyFacts: facts},
		facts.HTTP != 0 && possibleResource(ev.Type)
}

func describePhase(stage string, requests []requestFacts, responses map[network.RequestID]responseFacts, cutoff *cdp.MonotonicTime) phaseReport {
	report := phaseReport{Stage: stage, Status: "no_evidence", Observations: []networkObservation{}}
	// A redirect reuses Chromium's request ID. Each new request event carries
	// the previous hop's response. The response event closes the last hop.
	last := make(map[network.RequestID]int, len(requests))
	redirects := make(map[int]replyFacts)
	for i, req := range requests {
		if prior, exists := last[req.id]; exists && req.redirected.HTTP != 0 {
			redirects[prior] = req.redirected
		}
		last[req.id] = i
	}
	ids := make(map[network.RequestID]int)
	hops := make(map[network.RequestID]int)
	routes := make(map[[sha256.Size]byte]int)
	for i, req := range requests {
		ids[req.id] = alias(ids, req.id)
		hops[req.id]++
		if req.method == "" {
			continue
		}
		facts := redirects[i]
		resource := req.resource
		if i == last[req.id] {
			if response, exists := responses[req.id]; exists && response.at >= req.at &&
				(cutoff == nil || response.at < *cutoff) {
				facts, resource = response.replyFacts, response.resource
			}
		}
		if facts.HTTP == 0 {
			report.UnobservedResponses++
			continue
		}
		if !observableResource(resource, facts) {
			continue
		}
		routes[req.route] = alias(routes, req.route)
		report.Observations = append(report.Observations, networkObservation{
			Request: ids[req.id], Hop: hops[req.id], Route: routes[req.route], Method: req.method,
			Relation: req.relation, Resource: string(resource), RangeRequested: req.hasRange, replyFacts: facts,
		})
	}
	if len(report.Observations) != 0 {
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
	return replyFacts{
		HTTP: response.Status, Attachment: strings.EqualFold(strings.TrimSpace(kind), "attachment"),
		RangeAdvertised: strings.EqualFold(strings.TrimSpace(header(response.Headers, "Accept-Ranges")), "bytes"),
		ContentRange:    header(response.Headers, "Content-Range") != "",
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
