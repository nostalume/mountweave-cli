package cli

import (
	"errors"
	"io"
	"testing"

	core "github.com/nostalume/mountweave"
)

// checkWebsiteRange checks the same original and range relation across sources.
// The source fixture controls how the original's signed body is reached.
func checkWebsiteRange(t *testing.T, c *core.Core, path string, original string) {
	t.Helper()
	total := int64(len(original))
	want := core.ContentRange{Start: 1, End: total - 2, Total: total}
	options := core.OpenOptions{Range: &core.Range{Start: want.Start, End: want.End}}
	opened, err := c.Open(t.Context(), localActor, core.Path(path), options)
	if err != nil {
		t.Fatalf("open nonzero range: %v", err)
	}
	part, readErr := io.ReadAll(opened.Body)
	if err := errors.Join(readErr, opened.Body.Close()); err != nil {
		t.Fatalf("read or close range: %v", err)
	}
	if opened.Range == nil || *opened.Range != want || opened.Length == nil ||
		*opened.Length != want.End-want.Start+1 || string(part) != original[want.Start:want.End+1] {
		t.Fatal("nonzero range did not reconcile with the original")
	}
}
