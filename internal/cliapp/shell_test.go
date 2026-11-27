package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/nostalume/mountweave"
)

type interruptTestDriver struct {
	core.Driver
	started chan struct{}
	closes  int
}

func (d *interruptTestDriver) Stat(ctx context.Context, target core.NativeAddress, _ ...core.ReadSelection) (core.Metadata, error) {
	if target.Value() == "/blocked" {
		close(d.started)
		<-ctx.Done()
		return core.Metadata{}, ctx.Err()
	}
	return core.Metadata{Name: "ready", Kind: core.File}, nil
}

func (d *interruptTestDriver) Close() error {
	d.closes++
	return nil
}

func TestShellInterruptLeavesMountOpen(t *testing.T) {
	driver := &interruptTestDriver{started: make(chan struct{})}
	opens := 0
	fixture, err := NewSource("shell-fixture", "Shell fixture", nil, func(struct{}, string) (PreparedSource, error) {
		return PreparedSource{Open: func(context.Context, string) (core.Driver, error) {
			opens++
			return driver, nil
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApp(nil, fixture)
	if err != nil {
		t.Fatal(err)
	}
	saved := ConfigSnapshot{Path: filepath.Join(t.TempDir(), "settings.json"), Value: Config{
		Mounts: []MountRecord{{At: "/fixture", Driver: "shell-fixture", Options: json.RawMessage("{}")}},
	}}
	var out, diagnostics bytes.Buffer
	interrupts := make(chan os.Signal, 1)
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	done := make(chan int, 1)
	go func() {
		input := strings.NewReader("stat /fixture//ready\nstat /fixture/blocked\nstat /fixture/ready\nexit\n")
		done <- app.runShell(ctx, saved, arguments{}, input, &out, &diagnostics, interrupts)
	}()
	select {
	case <-driver.started:
		interrupts <- os.Interrupt
	case <-ctx.Done():
		t.Fatal("shell did not start the first file operation")
	}
	select {
	case code := <-done:
		if code != 0 || opens != 1 || driver.closes != 1 || strings.Count(out.String(), "/fixture/ready") != 1 ||
			!strings.Contains(diagnostics.String(), "cancelled:") || !strings.Contains(diagnostics.String(), "invalid_path:") {
			t.Fatalf("shell did not recover using the same mount: code=%d opens=%d closes=%d\n%s\n%s",
				code, opens, driver.closes, &out, &diagnostics)
		}
	case <-ctx.Done():
		t.Fatal("shell did not finish after interrupt")
	}
}
