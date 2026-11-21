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
	"os"
	"strconv"
	"strings"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
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
	return &fileSample{path: path, size: int64(data.Len()), digest: sha256.Sum256(data.Bytes())}, nil
}

func (s *fileSample) compare(ctx context.Context, id network.RequestID) string {
	response, err := chromedp.Call(ctx, network.GetResponseBody, network.GetResponseBodyParams{RequestID: id})
	// CDP can return text decoded by the browser. Only its binary form
	// preserves the bytes needed for exact comparison.
	if err != nil || !response.Base64encoded || int64(len(response.Body)) > maxSampleResponseBytes {
		return "unavailable"
	}
	if int64(len(response.Body)) != s.size || sha256.Sum256(response.Body) != s.digest {
		return "mismatch"
	}
	return "match"
}

func printSampleCheck(out io.Writer, number int, requests []networkObservation) error {
	byResult := make(map[string][]string)
	for _, request := range requests {
		if request.SampleCheck != "" {
			byResult[request.SampleCheck] = append(byResult[request.SampleCheck], strconv.Itoa(request.Request))
		}
	}
	var results []string
	for _, result := range []struct{ state, label string }{
		{"match", "matched"},
		{"mismatch", "different bytes"},
		{"unavailable", "body unavailable"},
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
