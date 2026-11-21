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
	"slices"
	"strings"
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

func run(ctx context.Context, args []string, in io.Reader, out, prompts io.Writer) (err error) {
	flags := flag.NewFlagSet("mweave-browser-check", flag.ContinueOnError)
	flags.SetOutput(prompts)
	signIn := flags.Bool("signin", false, "Open the website sign-in window using the saved session")
	fresh := flags.Bool("fresh", false, "Sign in with a disposable session")
	withSample := flags.Bool("sample", false, "Generate a test file and check which download responses match it")
	flags.Usage = func() {
		fmt.Fprintln(prompts, "Usage: mweave-browser-check [options] https://cloud.example/")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return errors.New("Provide one website.")
	}
	entry, err := url.Parse(flags.Arg(0))
	if err != nil || entry.Scheme != "https" || entry.Hostname() == "" || entry.User != nil ||
		entry.RawQuery != "" || entry.ForceQuery {
		return errors.New("Use a public HTTPS entry URL without credentials or query parameters.")
	}
	var sample *fileSample
	if *withSample {
		dir, tempErr := os.MkdirTemp("", "mweave-sample-")
		if tempErr != nil {
			return errors.New("Could not create a temporary sample directory.")
		}
		defer func() {
			if os.RemoveAll(dir) != nil {
				err = errors.Join(err, errors.New("Could not remove the local sample."))
			}
		}()
		sample, err = createSample(filepath.Join(dir, filepath.Base(dir)+".zip"))
		if err != nil {
			return err
		}
	}
	report, checkErr := observeWebsite(ctx, entry, *signIn, *fresh, sample, bufio.NewReader(in), prompts)
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return errors.Join(checkErr, encoder.Encode(report))
}

func observeWebsite(ctx context.Context, entry *url.URL, signIn, fresh bool, sample *fileSample, in *bufio.Reader, out io.Writer) (report browserReport, err error) {
	report.Observations = []operationReport{}
	binary, options := website.Browser()
	if binary == "" {
		return report, errors.New("No compatible browser is available for an isolated website session.")
	}
	profile, existed, releaseProfile, err := openProbeProfile(entry, fresh)
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, releaseProfile()) }()
	if !existed || signIn || fresh {
		fmt.Fprintln(out, "Sign in to the website, then close the sign-in window.")
		if err := website.SignIn(ctx, binary, profile, entry.String(), func() error {
			return waitForEnter(ctx, in, out, "Press Enter after signing in and closing the window.")
		}, out); err != nil {
			return report, err
		}
	} else {
		fmt.Fprintln(out, "Reopening the saved browser session. If the site asks you to sign in, rerun with -signin.")
	}
	tab, closeTab, err := website.Launch(ctx, binary, append(options, chromedp.VisibleWindow), profile)
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, closeTab()) }()
	life, cancel := context.WithCancel(tab)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	if _, err := chromedp.CallBrowser(life, browser.SetDownloadBehavior, browser.SetDownloadBehaviorParams{
		Behavior: browser.SetDownloadBehaviorBehaviorDeny,
	}); err != nil {
		return report, errors.New("Could not disable local saving in the observation window.")
	}
	opening, err := capture(life, entry, nil, func() (bool, error) {
		navigation, done := context.WithTimeout(life, 15*time.Second)
		defer done()
		if _, err := chromedp.Run(navigation, chromedp.Navigate(entry.String())); err != nil {
			return false, errors.New("Could not reopen the website.")
		}
		return true, nil
	})
	report.Opening = &opening
	if err != nil {
		return report, err
	}
	if sample != nil {
		fmt.Fprintf(out, "Upload the generated test file, then press Enter.\n  %s\n"+
			"Click Download for that file and press Enter again. Remove the uploaded test file when finished.\n", sample.path)
	}
	report.Observations, err = observeOperations(life, in, out, sample != nil, func(work func() (bool, error)) (networkReport, error) {
		return capture(life, entry, sample, work)
	})
	return report, err
}

const operationLabels = "list details search download upload rename move delete other"
const visibleResults = "browser preview client membership denied unknown"

var (
	operations = strings.Fields(operationLabels)
	results    = strings.Fields(visibleResults)
)

func observeOperations(ctx context.Context, in *bufio.Reader, out io.Writer, checkSample bool, captureWindow func(func() (bool, error)) (networkReport, error)) ([]operationReport, error) {
	observations := []operationReport{}
	if _, err := fmt.Fprintln(out, "Use files you own. For changes, use a disposable file. The observation window blocks saving downloads.\n"+
		"Optional annotation example: delete denied."); err != nil {
		return observations, err
	}
	for {
		var observation operationReport
		var done bool
		evidence, err := captureWindow(func() (bool, error) {
			var err error
			observation, done, err = readObservation(ctx, in, out, len(observations)+1)
			return !done, err
		})
		if err != nil {
			return observations, err
		}
		if done {
			return observations, nil
		}
		observation.networkReport = evidence
		observations = append(observations, observation)
		if checkSample {
			if err := printSampleCheck(out, len(observations), evidence.Requests); err != nil {
				return observations, err
			}
		}
	}
}

func waitForEnter(ctx context.Context, in *bufio.Reader, out io.Writer, prompt string) error {
	_, err := promptLine(ctx, in, out, prompt)
	return err
}

func readObservation(ctx context.Context, in *bufio.Reader, out io.Writer, number int) (operationReport, bool, error) {
	prompt := fmt.Sprintf("Observation %d. Perform an action, then press Enter to mark it. Type q and press Enter to finish.", number)
	for {
		answer, err := promptLine(ctx, in, out, prompt)
		if err != nil {
			return operationReport{}, false, err
		}
		if strings.TrimSpace(answer) == "q" {
			return operationReport{}, true, nil
		}
		fields := strings.Fields(strings.ToLower(answer))
		if len(fields) == 0 {
			return operationReport{}, false, nil
		}
		if len(fields) <= 2 && slices.Contains(operations, fields[0]) {
			if len(fields) == 1 {
				return operationReport{Operation: fields[0]}, false, nil
			}
			if slices.Contains(results, fields[1]) {
				return operationReport{Operation: fields[0], ReportedResult: fields[1]}, false, nil
			}
		}
		prompt = "Press Enter to mark, or type q and press Enter to finish.\n" +
			"Operations [" + operationLabels + "]\nOptional result [" + visibleResults + "]"
	}
}

func promptLine(ctx context.Context, in *bufio.Reader, out io.Writer, prompt string) (string, error) {
	if _, err := fmt.Fprintln(out, prompt); err != nil {
		return "", err
	}
	type answer struct {
		line string
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		line, err := in.ReadSlice('\n')
		done <- answer{line: string(line), err: err}
	}()
	select {
	case answer := <-done:
		return answer.line, answer.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
