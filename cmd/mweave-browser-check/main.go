package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
	"github.com/nostalume/mountweave-cli/internal/website"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, in io.Reader, out, prompts io.Writer) error {
	flags := flag.NewFlagSet("mweave-browser-check", flag.ContinueOnError)
	flags.SetOutput(prompts)
	stage := flags.String("stage", "", "Observe only list, full, or range after reopening")
	flags.Usage = func() {
		fmt.Fprintln(prompts, "Usage: mweave-browser-check [-stage list|full|range] https://cloud.example/")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 || (*stage != "" && *stage != "list" && *stage != "full" && *stage != "range") {
		flags.Usage()
		return errors.New("Provide a website and, optionally, one stage.")
	}
	entry, err := url.Parse(flags.Arg(0))
	if err != nil || entry.Scheme != "https" || entry.Hostname() == "" || entry.User != nil ||
		entry.RawQuery != "" || entry.ForceQuery {
		return errors.New("Use a public HTTPS entry URL without credentials or query parameters.")
	}
	phases, checkErr := observeWebsite(ctx, entry, *stage, bufio.NewReader(in), prompts)
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return errors.Join(checkErr, encoder.Encode(struct {
		Phases []phaseReport `json:"phases"`
	}{Phases: phases}))
}

func observeWebsite(ctx context.Context, entry *url.URL, stage string, in *bufio.Reader, out io.Writer) (phases []phaseReport, err error) {
	phases = make([]phaseReport, 0, 3)
	binary, options := website.Browser()
	if binary == "" {
		return phases, errors.New("No compatible browser is available for an isolated website session.")
	}
	tmp, err := os.MkdirTemp("", "mweave-browser-check-")
	if err != nil {
		return phases, errors.New("Could not create the isolated browser directory.")
	}
	defer func() {
		if os.RemoveAll(tmp) != nil {
			err = errors.Join(err, errors.New("Could not remove the isolated browser directory."))
		}
	}()
	profile := filepath.Join(tmp, "profile")
	if err := website.PrivateDirectory(profile, true); err != nil {
		return phases, errors.New("Could not protect the isolated browser profile.")
	}
	fmt.Fprintln(out, "Sign in in the isolated window, then close that window.")
	if err := website.SignIn(ctx, binary, profile, entry.String(), func() error {
		return waitForEnter(ctx, in, out, "Press Enter after signing in and closing the window.")
	}, out); err != nil {
		return phases, err
	}
	tab, closeTab, err := website.Launch(ctx, binary, append(options, chromedp.VisibleWindow), profile)
	if err != nil {
		return phases, err
	}
	defer func() { err = errors.Join(err, closeTab()) }()
	life, cancel := context.WithCancel(tab)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	if _, err := chromedp.CallBrowser(life, browser.SetDownloadBehavior, browser.SetDownloadBehaviorParams{
		Behavior: browser.SetDownloadBehaviorBehaviorDeny,
	}); err != nil {
		return phases, errors.New("Could not disable local saving in the observation window.")
	}
	phase, err := capture(life, "reopen", entry, func() error {
		navigation, done := context.WithTimeout(life, 15*time.Second)
		defer done()
		if _, err := chromedp.Run(navigation, chromedp.Navigate(entry.String())); err != nil {
			return errors.New("Could not reopen the website.")
		}
		return nil
	})
	phases = append(phases, phase)
	if err != nil {
		return phases, err
	}
	for _, step := range []struct{ stage, prompt string }{
		{"list", "Open a private folder and advance to the next page if available. Press Enter when finished."},
		{"full", "Click Download on one small file. This window records the response without saving the file. Press Enter when the request ends."},
		{"range", "Perform the website action that requests part of an existing file, then press Enter. If the site has no such action, press Enter to end the observation."},
	} {
		if stage != "" && stage != step.stage || stage == "" && step.stage == "range" {
			continue
		}
		phase, err := capture(life, step.stage, entry, func() error { return waitForEnter(life, in, out, step.prompt) })
		phases = append(phases, phase)
		if err != nil {
			return phases, err
		}
	}
	return phases, nil
}

func waitForEnter(ctx context.Context, in *bufio.Reader, out io.Writer, prompt string) error {
	if _, err := fmt.Fprintln(out, prompt); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		_, err := in.ReadSlice('\n')
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
