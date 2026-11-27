package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"iter"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/debugger"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const (
	maxPhaseObservations = 64
	maxRelatedTabs       = 8
)

type browserReport struct {
	DownloadSaving string            `json:"download_saving,omitempty"`
	Opening        *networkReport    `json:"opening,omitempty"`
	Observations   []operationReport `json:"observations"`
	FileEffects    []string          `json:"file_effects,omitempty"`
	GeneratedFiles []string          `json:"generated_files,omitempty"`
}

type operationReport struct {
	Operation      string `json:"operation,omitempty"`
	ReportedResult string `json:"reported_result,omitempty"`
	networkReport
}

type networkReport struct {
	Downloads            []string             `json:"downloads,omitempty"`
	Navigation           []string             `json:"navigation,omitempty"`
	PageNotices          []string             `json:"page_notices,omitempty"`
	UnlinkedSampleChecks []string             `json:"unlinked_sample_checks,omitempty"`
	SampleStreamsOpened  int                  `json:"sample_streams_opened,omitempty"`
	Status               string               `json:"status"`
	Requests             []networkObservation `json:"requests"`
	UnobservedResponses  int                  `json:"unobserved_responses,omitempty"`
	Truncated            bool                 `json:"truncated,omitempty"`
}

type networkObservation struct {
	Request         int             `json:"request"`
	RelatedTab      int             `json:"related_tab,omitempty"`
	Hop             int             `json:"hop"`
	Route           int             `json:"route"`
	Method          string          `json:"method"`
	Relation        string          `json:"relation"`
	Resource        string          `json:"resource"`
	RangeRequested  bool            `json:"range_requested"`
	Transfer        string          `json:"transfer,omitempty"`
	BodyCandidate   bool            `json:"body_candidate,omitempty"`
	SampleCheck     string          `json:"sample_check,omitempty"`
	Endpoint        string          `json:"endpoint,omitempty"`
	RequestJSON     json.RawMessage `json:"request_json,omitempty"`
	RequestForm     json.RawMessage `json:"request_form,omitempty"`
	Operation       string          `json:"operation,omitempty"`
	Task            *taskEvidence   `json:"task,omitempty"`
	taskKey         [sha256.Size]byte
	ResponseJSON    json.RawMessage `json:"response_json,omitempty"`
	RequestCapture  string          `json:"request_capture,omitempty"`
	ResponseCapture string          `json:"response_capture,omitempty"`

	Initiator *requestInitiator `json:"initiator,omitempty"`
	replyFacts
}

type replyFacts struct {
	HTTP               int64  `json:"http,omitempty"`
	Attachment         bool   `json:"attachment"`
	RangeAdvertised    bool   `json:"range_advertised"`
	ContentRange       bool   `json:"content_range_present"`
	ContentKind        string `json:"content_kind,omitempty"`
	ContentDisposition string `json:"content_disposition,omitempty"`
}

// Request IDs are assigned by a target session. Only the main tab's IDs can
// be used to read sample bodies from the main tab.
type observedRequest struct {
	tab int
	id  network.RequestID
}

type requestFacts struct {
	wallTime       cdp.TimeSinceEpoch
	download       downloadTarget
	id             observedRequest
	at             cdp.MonotonicTime
	route          [sha256.Size]byte
	method         string
	relation       string
	resource       network.ResourceType
	hasRange       bool
	redirected     replyFacts
	endpoint       string
	requestJSON    json.RawMessage
	requestForm    json.RawMessage
	operation      string
	taskKey        [sha256.Size]byte
	requestCapture string
	initiator      []scriptPosition
}

type responseFacts struct {
	id       observedRequest
	at       cdp.MonotonicTime
	resource network.ResourceType
	capture  bool
	priority bool
	body     responseCapture
	replyFacts
}

type transferFacts struct {
	id    observedRequest
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

type downloadTarget struct {
	frame cdp.FrameID
	url   [sha256.Size]byte
}

type downloadEvent struct {
	guid   string
	target downloadTarget
	state  string
}

type recordedPhase struct {
	end        cdp.TimeSinceEpoch
	requests   recentEvents[requestFacts]
	responses  recentEvents[responseFacts]
	transfers  recentEvents[transferFacts]
	downloads  recentEvents[downloadEvent]
	navigation recentEvents[string]
	notices    recentEvents[string]
}

func (p *recordedPhase) truncated() bool {
	return p.requests.truncated || p.responses.truncated || p.transfers.truncated || p.downloads.truncated || p.navigation.truncated || p.notices.truncated
}

type recording struct {
	ctx     context.Context
	listen  context.Context
	stop    context.CancelFunc
	sample  *fileSample
	details *diagnosticCapture

	mu        sync.Mutex
	listeners sync.WaitGroup
	phases    []*recordedPhase

	sampleReads    sync.WaitGroup
	sampleStopping bool
	sampleReceipts map[observedRequest]sampleReceipt
	sampleErr      error
}

func watchEvents[E any](r *recording, events iter.Seq2[E, error], observe func(E)) {
	r.listeners.Go(func() {
		for event, err := range events {
			if err != nil {
				return
			}
			observe(event)
		}
	})
}

func recordEvents[E, T any](r *recording, events iter.Seq2[E, error], selectBuffer func(*recordedPhase) *recentEvents[T], project func(E) (T, bool)) {
	watchEvents(r, events, func(event E) {
		if facts, ok := project(event); ok {
			r.mu.Lock()
			selectBuffer(r.phases[len(r.phases)-1]).add(facts)
			r.mu.Unlock()
		}
	})
}

// startRecording observes one isolated browser continuously. Marks partition
// requests without detaching listeners or interrupting transfer.
func startRecording(ctx context.Context, entry *url.URL, sample *fileSample, origins diagnosticOrigins) (*recording, error) {
	origin := requestOrigin(entry)
	if origin == "" {
		return nil, errors.New("the browser entry must have an HTTP origin")
	}
	listen, stop := context.WithCancel(ctx)
	r := &recording{ctx: ctx, listen: listen, stop: stop, sample: sample, phases: []*recordedPhase{{}}}
	if len(origins) != 0 {
		r.details = newDiagnosticCapture(origins)
	}
	recordEvents(r, chromedp.BrowserEvents(listen, browser.DownloadWillBegin), func(p *recordedPhase) *recentEvents[downloadEvent] {
		return &p.downloads
	}, func(ev browser.EventDownloadWillBegin) (downloadEvent, bool) {
		return downloadEvent{guid: ev.GUID, target: newDownloadTarget(ev.FrameID, ev.URL), state: "started"}, ev.GUID != ""
	})
	recordEvents(r, chromedp.BrowserEvents(listen, browser.DownloadProgress), func(p *recordedPhase) *recentEvents[downloadEvent] {
		return &p.downloads
	}, func(ev browser.EventDownloadProgress) (downloadEvent, bool) {
		terminal := ev.State == browser.DownloadProgressStateCompleted || ev.State == browser.DownloadProgressStateCanceled
		return downloadEvent{guid: ev.GUID, state: string(ev.State)}, ev.GUID != "" && terminal
	})
	if _, err := chromedp.CallBrowser(ctx, browser.SetDownloadBehavior, browser.SetDownloadBehaviorParams{
		Behavior: browser.SetDownloadBehaviorBehaviorDeny, EventsEnabled: new(true),
	}); err != nil {
		stop()
		r.listeners.Wait()
		return nil, errors.Join(errors.New("could not configure browser download observation"), ctx.Err())
	}
	if err := r.observeNetwork(listen, origin, 0, sample != nil); err != nil {
		stop()
		r.listeners.Wait()
		_ = r.disable()
		return nil, errors.Join(errors.New("could not start network observation"), ctx.Err())
	}
	r.observeNavigation(origin)
	r.observePageNotices(entry)
	if err := r.armSample(); err != nil {
		_, finishErr := r.finish()
		return nil, errors.Join(err, finishErr)
	}
	return r, nil
}

func (r *recording) observeNetwork(ctx context.Context, origin string, tab int, sample bool) error {
	recordEvents(r, chromedp.Events(ctx, network.RequestWillBeSent), func(p *recordedPhase) *recentEvents[requestFacts] {
		return &p.requests
	}, func(ev network.EventRequestWillBeSent) (requestFacts, bool) {
		facts, ok := projectRequest(origin, tab, ev)
		if ok && r.details != nil {
			r.details.describeRequest(&facts, ev)
		}
		return facts, ok
	})
	recordEvents(r, chromedp.Events(ctx, network.ResponseReceived), func(p *recordedPhase) *recentEvents[responseFacts] {
		return &p.responses
	}, func(ev network.EventResponseReceived) (responseFacts, bool) {
		facts, ok := projectResponse(tab, ev)
		if ok && r.details != nil {
			r.details.response(r, ctx, &facts, ev)
		}
		return facts, ok
	})
	recordEvents(r, chromedp.Events(ctx, network.LoadingFinished), func(p *recordedPhase) *recentEvents[transferFacts] {
		return &p.transfers
	}, func(ev network.EventLoadingFinished) (transferFacts, bool) {
		id := observedRequest{tab, ev.RequestID}
		if r.details != nil {
			r.details.complete(r, ctx, id, ev.EncodedDataLength)
		}
		return transferFacts{id: id, at: ev.Timestamp, state: "finished"}, true
	})
	recordEvents(r, chromedp.Events(ctx, network.LoadingFailed), func(p *recordedPhase) *recentEvents[transferFacts] {
		return &p.transfers
	}, func(ev network.EventLoadingFailed) (transferFacts, bool) {
		facts, ok := projectFailure(tab, ev)
		if r.details != nil {
			r.details.failed(facts.id)
		}
		return facts, ok
	})
	// Chromium treats zero as an unlimited request-body event budget.
	post, single, total := int64(1), int64(0), int64(0)
	if r.details != nil {
		post, single, total = maxDiagnosticBodyBytes, maxDiagnosticBodyBytes, maxDiagnosticBodyBytes*diagnosticBodyLimit
		if _, err := chromedp.Call(ctx, debugger.Enable, debugger.EnableParams{}); err == nil {
			if _, err := chromedp.Call(ctx, debugger.SetSkipAllPauses, debugger.SetSkipAllPausesParams{Skip: true}); err != nil {
				_, _ = chromedp.Call(ctx, debugger.Disable, cdp.Empty{})
			}
		}
	}
	if sample {
		single = max(single, maxSampleResponseBytes)
		total += maxSampleResponseBytes * maxSampleResponses
	}
	params := network.EnableParams{MaxPostDataSize: &post}
	if single != 0 {
		params.MaxResourceBufferSize, params.MaxTotalBufferSize = &single, &total
	}
	_, err := chromedp.Call(ctx, network.Enable, params)
	return err
}

func (r *recording) observeNavigation(origin string) {
	// Launch has already enabled the Page domain and target discovery.
	buffer := func(phase *recordedPhase) *recentEvents[string] { return &phase.navigation }
	recordEvents(r, chromedp.Events(r.listen, page.WindowOpen), buffer, func(ev page.EventWindowOpen) (string, bool) {
		if externalProtocol(ev.URL) {
			return "external_protocol_requested", true
		}
		return "window_open_requested", true
	})
	recordEvents(r, chromedp.Events(r.listen, page.FrameRequestedNavigation), buffer, func(ev page.EventFrameRequestedNavigation) (string, bool) {
		return "external_protocol_requested", externalProtocol(ev.URL)
	})
	parent := chromedp.FromContext(r.ctx).Target.TargetID
	tabs := 0
	relatedTab := func(info *target.Info) int {
		if info == nil || info.Type != "page" || info.Subtype != "" || info.OpenerID != parent {
			return 0
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		navigation := &r.phases[len(r.phases)-1].navigation
		navigation.add("new_tab")
		if tabs == maxRelatedTabs {
			navigation.add("new_tab_unobserved")
			return 0
		}
		tabs++
		return tabs
	}
	watchEvents(r, chromedp.BrowserEvents(r.listen, target.TargetCreated), func(ev target.EventTargetCreated) {
		if r.sampling() {
			return
		}
		if n := relatedTab(ev.TargetInfo); n != 0 {
			r.observeRelatedTab(origin, ev.TargetInfo.TargetID, n, nil)
		}
	})
	if r.sample != nil {
		watchEvents(r, chromedp.BrowserEvents(r.listen, target.AttachedToTarget), func(ev target.EventAttachedToTarget) {
			if !ev.WaitingForDebugger {
				return
			}
			release := sync.OnceFunc(func() {
				cleanup, done := context.WithTimeout(context.WithoutCancel(r.ctx), 2*time.Second)
				defer done()
				_, _ = chromedp.CallBrowser(cleanup, target.DetachFromTarget, target.DetachFromTargetParams{SessionID: ev.SessionID})
			})
			if n := relatedTab(ev.TargetInfo); n != 0 {
				r.observeRelatedTab(origin, ev.TargetInfo.TargetID, n, release)
			} else {
				release()
			}
		})
	}
	recordEvents(r, chromedp.Events(r.listen, page.FrameNavigated), buffer, frameNavigation)
	tree, err := chromedp.Call(r.ctx, page.GetFrameTree, cdp.Empty{})
	if err == nil && tree.FrameTree != nil && tree.FrameTree.Frame != nil {
		frame := tree.FrameTree.Frame.ID
		recordEvents(r, chromedp.Events(r.listen, page.NavigatedWithinDocument), buffer, func(ev page.EventNavigatedWithinDocument) (string, bool) {
			return "same_document", ev.FrameID == frame
		})
	}
}

func frameNavigation(ev page.EventFrameNavigated) (string, bool) {
	if ev.Frame == nil || ev.Frame.UnreachableURL != "" {
		return "", false
	}
	u, err := url.Parse(ev.Frame.URL)
	if err != nil || requestOrigin(u) == "" {
		return "", false
	}
	if ev.Frame.ParentID != "" {
		return "iframe_document", true
	}
	return "same_tab", true
}

func externalProtocol(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "about", "blob", "data", "javascript", "chrome", "chrome-extension", "devtools", "file":
		return false
	default:
		return true
	}
}

func (r *recording) noteNavigation(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phases[len(r.phases)-1].navigation.add(event)
}

func (r *recording) sampling() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sampleReceipts != nil
}

func (r *recording) observeRelatedTab(origin string, id target.ID, number int, release func()) {
	r.listeners.Go(func() {
		owner, closeOwner := context.WithCancel(context.WithoutCancel(r.listen))
		defer closeOwner()
		tab, detach := chromedp.NewContext(owner, chromedp.WithTargetID(id), chromedp.WithDetachOnCancel())
		defer detach()
		stopSetup := context.AfterFunc(r.listen, closeOwner)
		defer stopSetup()
		life, cancel := context.WithCancel(tab)
		stopListening := context.AfterFunc(r.listen, cancel)
		var sampleDone <-chan struct{}
		defer func() {
			stopListening()
			cancel()
			if sampleDone != nil {
				// The sample still needs the attached session to close its stream.
				<-sampleDone
			}
		}()
		var timeout *time.Timer
		if release != nil {
			// A new page can start its request before TargetCreated is
			// handled. Hold navigation only while its listeners start.
			defer release()
			timeout = time.AfterFunc(2*time.Second, func() {
				closeOwner()
				release()
			})
			defer timeout.Stop()
		}
		closed := chromedp.BrowserEvents(life, target.TargetDestroyed)
		if err := chromedp.Do(tab); err != nil {
			if r.listen.Err() == nil {
				r.noteNavigation("new_tab_unobserved")
			}
			return
		}
		if !stopSetup() {
			return
		}
		err := r.observeNetwork(life, origin, number, false)
		if release != nil {
			if err == nil {
				sampleDone, err = r.observeSample(life, number)
			}
			release()
			_, resumeErr := chromedp.Call(life, runtime.RunIfWaitingForDebugger, cdp.Empty{})
			timeout.Stop()
			err = errors.Join(err, resumeErr)
		}
		if err != nil {
			if r.listen.Err() == nil {
				r.noteNavigation("new_tab_unobserved")
			}
			return
		}
		r.noteNavigation("new_tab_attached")
		for event, err := range closed {
			if err != nil || event.TargetID == id {
				return
			}
		}
	})
}

func (r *recording) mark() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phases[len(r.phases)-1].end = cdp.TimeSinceEpoch(float64(time.Now().UnixMicro()) / 1e6)
	r.phases = append(r.phases, &recordedPhase{})
}

func (r *recording) finish() ([]networkReport, error) {
	// Give terminal events a short opportunity to arrive once, at exit.
	ctx, done := context.WithTimeout(r.ctx, 300*time.Millisecond)
	<-ctx.Done()
	done()
	r.mu.Lock()
	r.sampleStopping = true
	r.mu.Unlock()
	// Keep selected responses alive until their bounded reads release them.
	r.sampleReads.Wait()
	var autoAttachErr error
	if r.sampleReceipts != nil {
		autoAttachErr = r.setSampleAutoAttach(false)
	}
	r.stop()
	r.listeners.Wait()
	if r.details != nil {
		for _, phase := range r.phases {
			for i := range phase.responses.count {
				response := &phase.responses.items[i]
				if response.capture {
					response.body = responseCapture{state: "unavailable"}
					if captured := r.details.responses[response.id]; captured.at == response.at && captured.state != "" {
						response.body = captured
					}
				}
			}
		}
	}

	compare := (func(network.RequestID) string)(nil)
	if r.sample != nil {
		check, cancel := context.WithTimeout(r.ctx, 2*time.Second)
		defer cancel()
		compare = func(id network.RequestID) string {
			if check.Err() != nil {
				return "not_checked"
			}
			return r.sample.compare(check, id)
		}
	}
	reports := describeRecording(r.phases, compare, r.sampleReceipts)
	var fetchErr error
	if r.sampleReceipts != nil {
		fetchErr = disableFetch(r.ctx)
	}
	return reports, errors.Join(r.sampleErr, autoAttachErr, fetchErr, r.disable())
}

func (r *recording) setSampleAutoAttach(active bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 2*time.Second)
	defer cancel()
	params := target.SetAutoAttachParams{
		AutoAttach: active, WaitForDebuggerOnStart: active, Flatten: new(true),
	}
	if active {
		params.Filter = target.Filter{{Type: "page"}, {Exclude: true}}
	}
	_, err := chromedp.CallBrowser(ctx, target.SetAutoAttach, params)
	if err != nil {
		return errors.New("could not configure sample tab observation")
	}
	return nil
}

func (r *recording) disable() error {
	cleanup, done := context.WithTimeout(context.WithoutCancel(r.ctx), 2*time.Second)
	defer done()
	if r.details != nil {
		_, _ = chromedp.Call(cleanup, debugger.Disable, cdp.Empty{})
	}
	if _, err := chromedp.Call(cleanup, network.Disable, cdp.Empty{}); err != nil && r.ctx.Err() == nil {
		return errors.New("could not stop network observation")
	}
	return nil
}

func newDownloadTarget(frame cdp.FrameID, rawURL string) downloadTarget {
	if frame == "" || rawURL == "" {
		return downloadTarget{}
	}
	return downloadTarget{frame: frame, url: sha256.Sum256([]byte(rawURL))}
}

type requestTrail struct {
	phase        int
	target       downloadTarget
	response     responseFacts
	transfer     transferFacts
	sample       sampleReceipt
	sampleLinked bool
}

func describeRecording(phases []*recordedPhase, compare func(network.RequestID) string, receipts map[observedRequest]sampleReceipt) []networkReport {
	requests := make([]recentEvents[requestFacts], len(phases))
	reports := make([]networkReport, len(phases))
	trails := make(map[observedRequest]*requestTrail)
	for i, phase := range phases {
		for _, request := range phase.requests.ordered() {
			trail := trails[request.id]
			if trail == nil {
				owner := i
				if request.wallTime != 0 {
					owner = sort.Search(len(phases)-1, func(i int) bool {
						return request.wallTime < phases[i].end
					})
				}
				trail = &requestTrail{phase: owner}
				trails[request.id] = trail
			}
			requests[trail.phase].add(request)
			trail.target = request.download
		}
	}

	// Network and Browser use different IDs. Move a browser start to a
	// request's action only when the final URL and frame have one match.
	targetOwners := make(map[downloadTarget]int)
	for id, trail := range trails {
		target := trail.target
		if target == (downloadTarget{}) {
			continue
		}
		if receipt, ok := receipts[id]; ok && receipt.target == target {
			trail.sample = receipt
		}
		if _, duplicate := targetOwners[target]; duplicate {
			targetOwners[target] = -1
		} else {
			targetOwners[target] = trail.phase
		}
	}
	downloadEnds := make(map[string]string)
	for _, phase := range phases {
		for _, response := range phase.responses.ordered() {
			if trail := trails[response.id]; trail != nil && response.at >= trail.response.at {
				trail.response = response
			}
		}
		for _, transfer := range phase.transfers.ordered() {
			if trail := trails[transfer.id]; trail != nil && transfer.at >= trail.transfer.at {
				trail.transfer = transfer
			}
		}
		for _, event := range phase.downloads.ordered() {
			if event.state != "started" {
				downloadEnds[event.guid] = event.state
			}
		}
	}
	for i := range phases {
		reports[i] = describeNetwork(requests[i].ordered(), trails, compare)
		reports[i].Truncated = requests[i].truncated || phases[i].truncated()
		reports[i].Navigation = phases[i].navigation.ordered()
		reports[i].PageNotices = phases[i].notices.ordered()
		if len(reports[i].Navigation) != 0 || len(reports[i].PageNotices) != 0 {
			reports[i].Status = "observed"
		}
	}
	for id, receipt := range receipts {
		owner, linked := receipt.phase, false
		if trail := trails[id]; trail != nil && trail.sampleLinked {
			owner, linked = trail.phase, true
		}
		report := &reports[owner]
		if receipt.streamOpened {
			report.SampleStreamsOpened++
		}
		if linked {
			continue
		}
		// The stream receipt is still valid if the bounded Network trace
		// lost its request. Preserve it without inventing a request or hop.
		report.UnlinkedSampleChecks = append(report.UnlinkedSampleChecks, receipt.check)
		report.Status = "observed"
	}
	for i := range reports {
		slices.Sort(reports[i].UnlinkedSampleChecks)
	}
	for i, phase := range phases {
		for _, event := range phase.downloads.ordered() {
			if event.state != "started" {
				continue
			}
			owner, matched := targetOwners[event.target]
			if !matched || owner < 0 || owner > i {
				owner = i
			}
			state := downloadEnds[event.guid]
			if state == "" {
				state = event.state
			}
			reports[owner].Downloads = append(reports[owner].Downloads, state)
			reports[owner].Status = "observed"
		}
	}
	linkTasks(reports)
	return reports
}

func projectRequest(entryOrigin string, tab int, ev network.EventRequestWillBeSent) (requestFacts, bool) {
	// An unobserved new destination may still finish the preceding hop.
	request := requestFacts{id: observedRequest{tab, ev.RequestID}, at: ev.Timestamp, wallTime: ev.WallTime, redirected: projectReply(ev.RedirectResponse)}
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
	if ev.Type == network.ResourceTypeDocument || ev.Type == network.ResourceTypeOther {
		request.download = newDownloadTarget(ev.FrameID, ev.Request.URL)
	}
	request.method, request.relation, request.resource = ev.Request.Method, relation, ev.Type
	request.hasRange = header(ev.Request.Headers, "Range") != ""
	return request, true
}

func projectResponse(tab int, ev network.EventResponseReceived) (responseFacts, bool) {
	facts := projectReply(ev.Response)
	return responseFacts{id: observedRequest{tab, ev.RequestID}, at: ev.Timestamp, resource: ev.Type, replyFacts: facts},
		facts.HTTP != 0 && possibleResource(ev.Type)
}

func projectFailure(tab int, ev network.EventLoadingFailed) (transferFacts, bool) {
	state := "failed"
	if ev.Canceled {
		state = "canceled"
	}
	return transferFacts{id: observedRequest{tab, ev.RequestID}, at: ev.Timestamp, state: state}, possibleResource(ev.Type)
}

func describeNetwork(requests []requestFacts, trails map[observedRequest]*requestTrail, compare func(network.RequestID) string) networkReport {
	report := networkReport{Status: "no_evidence", Requests: []networkObservation{}}
	// A redirect reuses Chromium's request ID. Each new request event carries
	// the previous hop's response. Response and transfer events belong to
	// the last hop only if they are current for that request.
	last := make(map[observedRequest]int, len(requests))
	redirects := make(map[int]replyFacts)
	var firstRequest cdp.MonotonicTime
	for i, req := range requests {
		if req.at > 0 && (firstRequest == 0 || req.at < firstRequest) {
			firstRequest = req.at
		}
		if prior, exists := last[req.id]; exists && req.redirected.HTTP != 0 {
			redirects[prior] = req.redirected
		}
		last[req.id] = i
	}
	ids := make(map[observedRequest]int)
	hops := make(map[observedRequest]int)
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
		var transfer, sampleCheck string
		var body responseCapture
		if i == last[req.id] {
			trail := trails[req.id]
			if response := trail.response; response.HTTP != 0 && response.at >= req.at {
				facts, resource = response.replyFacts, response.resource
				if req.endpoint != "" {
					body = response.body
				}
			}
			if trail.sample.check != "" && (facts.HTTP == 0 || facts.HTTP == http.StatusOK) {
				facts, sampleCheck = trail.sample.facts, trail.sample.check
				trail.sampleLinked = true
			}
			// Chromium can timestamp completion before the header event.
			// The last hop's request start is the correlation boundary.
			if end := trail.transfer; end.at >= req.at {
				transfer = end.state
			}
		}
		failed := transfer == "failed" || transfer == "canceled"
		if facts.HTTP == 0 {
			report.UnobservedResponses++
			if !failed && req.endpoint == "" {
				continue
			}
		}
		bodyCandidate := req.method == "GET" && (facts.HTTP == http.StatusOK || facts.HTTP == http.StatusPartialContent) &&
			(facts.Attachment || facts.HTTP == http.StatusPartialContent && req.hasRange && facts.ContentRange ||
				transfer == "finished" && facts.ContentKind == "binary")
		if sampleCheck == "" && compare != nil && i == last[req.id] && req.method == "GET" && facts.HTTP == http.StatusOK &&
			!req.hasRange && !facts.ContentRange && (facts.Attachment || facts.RangeAdvertised || facts.ContentKind == "binary") {
			switch {
			case req.id.tab == 0 && transfer == "finished":
				sampleCheck = "not_checked"
				candidates = append(candidates, sampleCandidate{requestID: req.id.id, index: len(report.Requests)})
			case facts.Attachment:
				sampleCheck = "headers_only"
			case req.id.tab == 0:
				sampleCheck = "unavailable"
			}
		}
		if !observableResource(resource, facts) && !failed && !bodyCandidate && sampleCheck == "" && req.endpoint == "" {
			continue
		}
		routes[req.route] = alias(routes, req.route)
		var initiator *requestInitiator
		if len(req.initiator) != 0 {
			initiator = &requestInitiator{Frames: req.initiator}
			if req.at > 0 {
				initiator.RequestOffsetMS = new(int64((req.at - firstRequest) * 1000))
			}
		}
		report.Requests = append(report.Requests, networkObservation{
			Request: ids[req.id], RelatedTab: req.id.tab, Hop: hops[req.id], Route: routes[req.route], Method: req.method,
			Relation: req.relation, Resource: string(resource), RangeRequested: req.hasRange,
			Transfer: transfer, BodyCandidate: bodyCandidate, SampleCheck: sampleCheck, replyFacts: facts,
			Endpoint: req.endpoint, RequestJSON: req.requestJSON, RequestForm: req.requestForm, RequestCapture: req.requestCapture,
			Operation: req.operation, taskKey: req.taskKey,
			ResponseJSON: body.body, ResponseCapture: body.state, Initiator: initiator,
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
	disposition := strings.TrimSpace(header(response.Headers, "Content-Disposition"))
	if disposition != "" {
		kind, _, _ := strings.Cut(disposition, ";")
		disposition = strings.ToLower(strings.TrimSpace(kind))
		switch disposition {
		case "attachment", "inline":
		default:
			disposition = "other"
		}
	}
	declaredType := response.MimeType
	if declaredType == "" {
		declaredType = header(response.Headers, "Content-Type")
	}
	return replyFacts{
		HTTP: response.Status, Attachment: disposition == "attachment", ContentDisposition: disposition,
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
