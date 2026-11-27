package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	streamio "github.com/chromedp/cdproto/io"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/nostalume/mountweave/browser"
)

const (
	maxSampleResponseBytes int64 = 64 << 10
	maxSampleResponses           = 4
)

type fileSample struct {
	path   string
	size   int64
	digest [sha256.Size]byte
}

func createSample(path string) (*fileSample, error) {
	const failure = "Could not write the temporary test file."
	var data bytes.Buffer
	archive := zip.NewWriter(&data)
	entry, err := archive.Create("sample.txt")
	if err != nil {
		return nil, errors.New(failure)
	}
	if _, err := io.WriteString(entry, rand.Text()); err != nil {
		return nil, errors.New(failure)
	}
	if err := archive.Close(); err != nil {
		return nil, errors.New(failure)
	}
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		return nil, errors.New(failure)
	}
	return sampleFromBytes(path, data.Bytes()), nil
}

func loadSample(path string) (*fileSample, error) {
	failure := errors.New("Use a local test ZIP no larger than 64 KiB.")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxSampleResponseBytes {
		return nil, failure
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, failure
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSampleResponseBytes+1))
	if err != nil || int64(len(data)) != info.Size() {
		return nil, failure
	}
	if _, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err != nil {
		return nil, failure
	}
	return sampleFromBytes(path, data), nil
}

func sampleFromBytes(path string, data []byte) *fileSample {
	return &fileSample{path: path, size: int64(len(data)), digest: sha256.Sum256(data)}
}

func (s *fileSample) compare(ctx context.Context, id network.RequestID) string {
	response, err := chromedp.Call(ctx, network.GetResponseBody, network.GetResponseBodyParams{RequestID: id})
	// CDP can return text decoded by the browser. Only its binary form
	// preserves the bytes needed for exact comparison.
	if err != nil || !response.Base64encoded || int64(len(response.Body)) > maxSampleResponseBytes {
		return "unavailable"
	}
	return s.match(response.Body)
}

func (s *fileSample) match(body []byte) string {
	if int64(len(body)) != s.size || sha256.Sum256(body) != s.digest {
		return "mismatch"
	}
	return "match"
}

type sampleReceipt struct {
	target       downloadTarget
	facts        replyFacts
	check        string
	phase        int
	streamOpened bool
}

// attachment selects only a final full response for the generated ZIP.
func (s *fileSample) attachment(event fetch.EventRequestPaused) (sampleReceipt, bool) {
	if event.NetworkID == "" || event.FrameID == "" || event.Request == nil ||
		event.Request.Method != http.MethodGet || event.ResponseStatusCode != http.StatusOK ||
		event.ResponseErrorReason != "" || header(event.Request.Headers, "Range") != "" {
		return sampleReceipt{}, false
	}
	headers := browser.ResponseHeaders(event.ResponseHeaders)
	disposition := headers.Values("Content-Disposition")
	length := headers.Values("Content-Length")
	encoding := headers.Values("Content-Encoding")
	// A response can omit its length. The stream still has to reach EOF
	// at the exact sample size before its digest can match.
	if len(disposition) != 1 || len(length) > 1 || len(length) == 1 && length[0] != strconv.FormatInt(s.size, 10) ||
		len(headers.Values("Content-Range")) != 0 ||
		len(encoding) != 0 && (len(encoding) != 1 || !strings.EqualFold(encoding[0], "identity")) ||
		s.size <= 0 || s.size > maxSampleResponseBytes {
		return sampleReceipt{}, false
	}
	kind, params, err := mime.ParseMediaType(disposition[0])
	if err != nil || kind != "attachment" || params["filename"] != filepath.Base(s.path) {
		return sampleReceipt{}, false
	}
	address, err := url.Parse(event.Request.URL)
	if err != nil || requestOrigin(address) == "" {
		return sampleReceipt{}, false
	}
	return sampleReceipt{
		target: newDownloadTarget(event.FrameID, event.Request.URL),
		facts: replyFacts{
			HTTP: http.StatusOK, Attachment: true, ContentDisposition: kind,
			RangeAdvertised: strings.EqualFold(strings.TrimSpace(headers.Get("Accept-Ranges")), "bytes"),
			ContentKind:     contentKind(headers.Get("Content-Type")),
		},
	}, true
}

func (s *fileSample) takeAttachment(ctx context.Context, id fetch.RequestID) (check string, opened bool, err error) {
	const failure = "could not verify the attachment stream"
	read, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	stream, takeErr := chromedp.Call(read, fetch.TakeResponseBodyAsStream, fetch.TakeResponseBodyAsStreamParams{RequestID: id})
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer done()
		var closeErr error
		if stream.Stream != "" {
			_, closeErr = chromedp.Call(cleanup, streamio.Close, streamio.CloseParams{Handle: stream.Stream})
		}
		// Taking the body consumes the paused response. End it without saving
		// even if the browser did not acknowledge the stream command.
		_, abortErr := chromedp.Call(cleanup, fetch.FailRequest, fetch.FailRequestParams{RequestID: id, ErrorReason: network.ErrorReasonAborted})
		if closeErr != nil || abortErr != nil {
			check, err = "unavailable", errors.New("could not release the sample attachment")
		}
	}()
	if takeErr != nil || stream.Stream == "" {
		return "stream_unavailable", false, nil
	}
	body := browser.NewBody(read, stream.Stream, func() error { return nil }, errors.New(failure), s.size, true)
	data, readErr := io.ReadAll(body)
	if readErr != nil {
		return "read_incomplete", true, nil
	}
	return s.match(data), true, nil
}

func (r *recording) armSample() error {
	r.mu.Lock()
	if r.sample == nil || r.sampleReceipts != nil {
		r.mu.Unlock()
		return nil
	}
	r.sampleReceipts = make(map[observedRequest]sampleReceipt)
	r.mu.Unlock()
	if err := r.setSampleAutoAttach(true); err != nil {
		return err
	}
	_, err := r.observeSample(r.listen, 0)
	return err
}

func (r *recording) observeSample(ctx context.Context, tab int) (<-chan struct{}, error) {
	done := make(chan struct{})
	events := chromedp.Events(ctx, fetch.RequestPaused)
	r.listeners.Go(func() {
		defer close(done)
		for event, err := range events {
			if err != nil {
				break
			}
			receipt, selected := r.sample.attachment(event)
			id := observedRequest{tab, event.NetworkID}
			if selected {
				r.mu.Lock()
				_, duplicate := r.sampleReceipts[id]
				selected = !r.sampleStopping && !duplicate && len(r.sampleReceipts) < maxSampleResponses
				if selected {
					receipt.phase = len(r.phases) - 1
					r.sampleReceipts[id] = receipt
					r.sampleReads.Add(1)
				}
				r.mu.Unlock()
			}
			if selected {
				receipt.check, receipt.streamOpened, err = r.sample.takeAttachment(ctx, event.RequestID)
				r.mu.Lock()
				r.sampleReceipts[id] = receipt
				r.mu.Unlock()
				r.sampleReads.Done()
			} else {
				_, err = chromedp.Call(ctx, fetch.ContinueResponse, fetch.ContinueResponseParams{RequestID: event.RequestID})
			}
			if err != nil && ctx.Err() == nil {
				release := disableFetch(ctx)
				r.mu.Lock()
				r.sampleErr = errors.Join(r.sampleErr, errors.New("could not finish sample response observation"), release)
				r.mu.Unlock()
				return
			}
		}
	})
	patterns := []*fetch.RequestPattern{
		{ResourceType: network.ResourceTypeDocument, RequestStage: fetch.RequestStageResponse},
		{ResourceType: network.ResourceTypeOther, RequestStage: fetch.RequestStageResponse},
	}
	if _, err := chromedp.Call(ctx, fetch.Enable, fetch.EnableParams{Patterns: patterns}); err != nil {
		return done, errors.Join(errors.New("could not start sample attachment observation"), ctx.Err())
	}
	return done, nil
}

func disableFetch(ctx context.Context) error {
	cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer done()
	if _, err := chromedp.Call(cleanup, fetch.Disable, cdp.Empty{}); err != nil {
		return errors.New("could not release sample response observation")
	}
	return nil
}

func printSampleCheck(out io.Writer, number int, report networkReport) error {
	byResult := make(map[string][]string)
	for _, request := range report.Requests {
		if request.SampleCheck != "" {
			byResult[request.SampleCheck] = append(byResult[request.SampleCheck], strconv.Itoa(request.Request))
		}
	}
	for _, check := range report.UnlinkedSampleChecks {
		byResult[check] = append(byResult[check], "request link unavailable")
	}
	var results []string
	for _, result := range []struct{ state, label string }{
		{"match", "full sample received and matched"},
		{"mismatch", "full sample differed"},
		{"headers_only", "attachment headers without byte evidence"},
		{"stream_unavailable", "attachment stream could not be opened"},
		{"read_incomplete", "attachment stream did not yield the exact length and EOF"},
		{"unavailable", "sample body unavailable"},
		{"not_checked", "not checked"},
	} {
		if ids := byResult[result.state]; len(ids) != 0 {
			results = append(results, result.label+" ["+strings.Join(ids, ", ")+"]")
		}
	}
	if len(results) == 0 {
		results = append(results, "no eligible response observed")
	}
	_, err := fmt.Fprintf(out, "Sample check %d. %s.\n", number, strings.Join(results, "; "))
	return err
}
