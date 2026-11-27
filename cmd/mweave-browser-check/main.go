package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/nostalume/mountweave-cli/internal/website"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var output io.Writer = os.Stdout
	var report bytes.Buffer
	if info, err := os.Stdout.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		output = &report
	}
	err := run(ctx, os.Args[1:], os.Stdin, output, os.Stderr)
	if report.Len() != 0 {
		dir, dirErr := probeReportDirectory(false, "")
		if dirErr == nil {
			dirErr = writeSavedProbeReport(os.Stderr, report.Bytes(), dir)
		}
		err = errors.Join(err, dirErr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func probeReportDirectory(detailed bool, reportDir string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "mweave-browser-check", "reports")
	if detailed {
		// Separate detailed reports from upgradable or shared anonymous report
		// directories. Files inherit the private directory ACL on Windows.
		dir = filepath.Join(cache, "mweave-browser-check-details")
	}
	if reportDir != "" {
		dir = reportDir
	}
	if detailed || reportDir != "" {
		if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
			return "", err
		}

		if err := website.PrivateDirectory(dir, true); err != nil {
			return "", errors.New("Could not open the private detailed report directory.")
		}
	}
	return dir, nil
}

func writeSavedProbeReport(out io.Writer, data []byte, dir string) error {
	path, err := saveProbeReport(dir, data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "Report saved:", path)
	return err
}

func saveProbeReport(dir string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, "report-*.json")
	if err != nil {
		return "", err
	}
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return "", errors.Join(err, os.Remove(file.Name()))
	}
	return file.Name(), nil
}

func run(ctx context.Context, args []string, in io.Reader, out, prompts io.Writer) (err error) {
	flags := flag.NewFlagSet("mweave-browser-check", flag.ContinueOnError)
	flags.SetOutput(prompts)
	signIn := flags.Bool("signin", false, "Open the website sign-in window using the saved session")
	fresh := flags.Bool("fresh", false, "Sign in with a disposable session")
	withSample := flags.Bool("sample", false, "Generate a test file and check which download responses match it")
	samplePath := flags.String("sample-file", "", "Compare download routes against an existing local test ZIP")
	withDownload := flags.Bool("download", false, "Observe download routes for an existing file")
	details := flags.String("details", "", "Save endpoints, small JSON and file-operation form fields for site or comma-separated HTTPS origins")
	changes := flags.Bool("changes", false, "Generate two test files and record file changes after sign-in")
	reportDir := flags.String("report-dir", "", "Save the report in this local directory")
	restoreSession := flags.Bool("restore-session", false, "Restore session cookies in the dedicated browser profile")
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
	if *changes {
		if *withDownload || *withSample || *samplePath != "" {
			return errors.New("Use -changes on its own or use a download check.")
		}
		if *details == "" {
			*details = "site"
		}
	}
	if *reportDir != "" {
		dir, err := filepath.Abs(*reportDir)
		if err != nil {
			return err
		}
		*reportDir = dir
	}
	origins, err := parseDiagnosticOrigins(*details, entry)
	if err != nil {
		return err
	}
	archiveDir := ""
	if len(origins) != 0 || *reportDir != "" {
		archiveDir, err = probeReportDirectory(len(origins) != 0, *reportDir)
		if err != nil {
			return err
		}
	}
	first := ""
	var sample *fileSample
	var changeFiles []string
	var changeFolder string
	switch {
	case *changes:
		dir, tempErr := os.MkdirTemp("", "mweave-changes-")
		if tempErr != nil {
			return errors.New("Could not create a temporary sample directory.")
		}
		var localEntries []string
		defer func() {
			for _, file := range slices.Backward(localEntries) {
				if removeErr := os.Remove(file); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					err = errors.Join(err, errors.New("Could not remove a local test file."))
				}
			}
			if os.Remove(dir) != nil {
				err = errors.Join(err, errors.New("Could not remove the temporary sample directory."))
			}
		}()
		for _, label := range []string{"A", "B"} {
			name := filepath.Join(dir, filepath.Base(dir)+"-"+label+".zip")
			changeFiles = append(changeFiles, name)
			localEntries = append(localEntries, name)
			if _, err := createSample(name); err != nil {
				return err
			}
		}
		folderParent := filepath.Join(dir, "folder-upload")
		changeFolder = filepath.Join(folderParent, filepath.Base(changeFiles[0]))
		for _, path := range []string{folderParent, changeFolder} {
			if err := os.Mkdir(path, 0700); err != nil {
				return errors.New("Could not create the temporary folder-upload sample.")
			}
			localEntries = append(localEntries, path)
		}
		placeholder := filepath.Join(changeFolder, "sample.zip")
		localEntries = append(localEntries, placeholder)
		if _, err := createSample(placeholder); err != nil {
			return err
		}
	case *samplePath != "":
		first = "download"
		sample, err = loadSample(*samplePath)
	case *withSample:
		first = "upload"
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
	case *withDownload:
		first = "download"
	}
	if err != nil {
		return err
	}
	report, checkErr := observeWebsite(ctx, entry, *signIn, *fresh, *restoreSession, sample, changeFiles, changeFolder, first, bufio.NewReader(in), prompts, origins)
	for _, name := range changeFiles {
		report.GeneratedFiles = append(report.GeneratedFiles, filepath.Base(name))
	}
	var local bytes.Buffer
	if archiveDir != "" {
		out = &local
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(report)
	if local.Len() != 0 && encodeErr == nil {
		return errors.Join(checkErr, writeSavedProbeReport(prompts, local.Bytes(), archiveDir))
	}
	return errors.Join(checkErr, encodeErr)
}

// attachFileEffects optionally registers provider hooks for diagnostic builds.
var attachFileEffects func(context.Context, *url.URL) func() []string

func observeWebsite(ctx context.Context, entry *url.URL, signIn, fresh, restoreSession bool, sample *fileSample, changeFiles []string, changeFolder, first string, in *bufio.Reader, out io.Writer, origins diagnosticOrigins) (report browserReport, err error) {
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
	changes := len(changeFiles) != 0
	if !changes && (!existed || signIn || fresh) {
		fmt.Fprintln(out, "Sign in to the website, then close the sign-in window.")
		if err := website.SignIn(ctx, binary, profile, entry.String(), func() error {
			return waitForEnter(ctx, in, out, "Press Enter after signing in and closing the window.")
		}, out); err != nil {
			return report, err
		}
	} else if !changes {
		fmt.Fprintln(out, "Reopening the saved browser session. If the site asks you to sign in, rerun with -signin.")
	}
	var browserOptions []chromedp.ContextOption
	if sample != nil {
		browserOptions = append(browserOptions, chromedp.WithErrorf(func(format string, args ...any) {
			// Auto-attach holds have no chromedp executor of their own.
			if format != "executor for %q doesn't exist" {
				log.Printf("ERROR: "+format, args...)
			}
		}))
	}
	launch := website.Launch
	if restoreSession {
		launch = website.LaunchWithSessionCookies
	}
	tab, closeTab, err := launch(ctx, binary, append(options, chromedp.VisibleWindow), profile, browserOptions...)
	if err != nil {
		return report, err
	}
	defer func() {
		disconnected := tab.Err() != nil
		closeErr := closeTab()
		if !disconnected {
			err = errors.Join(err, closeErr)
		}
	}()
	life, cancel := context.WithCancel(tab)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	closed := probeWindowClosed(life)
	go func() {
		<-closed
		cancel()
	}()
	open := func() error {
		navigation, done := context.WithTimeout(life, 15*time.Second)
		defer done()
		if err := chromedp.Do(navigation, chromedp.Navigate(entry.String())); err != nil {
			return errors.Join(errors.New("Could not reopen the website."), err)
		}
		return nil
	}
	if changes {
		if err := open(); err != nil {
			return report, err
		}
		answer, err := promptLine(life, in, out, "Sign in if needed. Press Enter once your files appear, or type q to skip this site.")
		if strings.TrimSpace(answer) == "q" || life.Err() != nil {
			return report, nil
		}
		if err != nil {
			return report, err
		}
	}
	record, err := startRecording(life, entry, sample, origins)
	if err != nil {
		return report, err
	}
	report.DownloadSaving = "blocked"
	passive := first == ""
	var fileEffects func() []string
	if passive && attachFileEffects != nil {
		fileEffects = attachFileEffects(record.listen, entry)
	}
	summary := out
	if passive {
		summary = io.Discard
	}
	defer func() {
		phases, finishErr := record.finish()
		if fileEffects != nil {
			report.FileEffects = fileEffects()
		}
		err = errors.Join(err, finishErr, report.complete(summary, phases, sample != nil))
	}()
	if !changes {
		navErr := open()
		record.mark()
		if navErr != nil {
			return report, navErr
		}
	} else {
		record.mark()
		fmt.Fprintln(out, "Recording file changes. Use these generated ZIPs in a new test folder.")
		for _, file := range changeFiles {
			fmt.Fprintln(out, " ", file)
		}
		fmt.Fprintln(out, "Reopen the folder after each test. For a recycle check, also open the recycle bin.")
		fmt.Fprintln(out, "Matching folder for a folder-upload check")
		fmt.Fprintln(out, " ", changeFolder)
		fmt.Fprintln(out, "Follow the test instructions for cleaning up the samples.")
	}
	if changes {
		err := waitForEnter(life, in, out, "Press Enter to save and finish this site. You can leave the browser open.")
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		return report, err
	}
	if passive {
		fmt.Fprintln(out, "Close the browser window or press Ctrl+C here to save the report.")
		<-closed
		return report, nil
	}
	if sample != nil {
		prompt, name := "Compare with the local sample %q.\n", filepath.Base(sample.path)
		if first == "upload" {
			prompt, name = "Generated test ZIP:\n  %s\nRemove the uploaded copy before finishing.\n", sample.path
		}
		if _, err := fmt.Fprintf(out, prompt, name); err != nil {
			return report, err
		}
	}
	report.Observations, err = observeOperations(life, in, out, first, record.mark)
	if life.Err() != nil && errors.Is(err, context.Canceled) {
		err = nil
	}
	return report, err
}

func probeWindowClosed(ctx context.Context) <-chan struct{} {
	page := chromedp.FromContext(ctx).Target.TargetID
	events := chromedp.BrowserEvents(ctx, target.TargetDestroyed)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for event, err := range events {
			if err != nil || event.TargetID == page {
				return
			}
		}
	}()
	return closed
}

func (r *browserReport) complete(out io.Writer, phases []networkReport, sample bool) error {
	r.Opening = &phases[0]
	var summary strings.Builder
	if len(r.Observations) == 0 {
		summary.WriteString("No action was marked.\n")
	} else {
		fmt.Fprintf(&summary, "Marked actions: %d.\n", len(r.Observations))
	}
	if r.Opening.Truncated {
		summary.WriteString("Opening evidence was truncated.\n")
	}
	tail := phases[len(phases)-1]
	if len(tail.Requests) != 0 || tail.UnobservedResponses != 0 || tail.Truncated ||
		len(tail.Downloads) != 0 || len(tail.Navigation) != 0 || len(tail.PageNotices) != 0 || len(tail.UnlinkedSampleChecks) != 0 {
		r.Observations = append(r.Observations, operationReport{})
		summary.WriteString("Unmarked activity was kept in the report.\n")
	}
	for i := range r.Observations {
		obs := &r.Observations[i]
		obs.networkReport = phases[i+1]
		var evidence []string
		attachments, bodyCandidates := 0, 0
		comparedAttachment, hasSample := false, len(obs.UnlinkedSampleChecks) != 0
		for _, req := range obs.Requests {
			if req.Method == http.MethodGet && req.Attachment && (req.HTTP == http.StatusOK || req.HTTP == http.StatusPartialContent) {
				attachments++
				comparedAttachment = comparedAttachment || req.SampleCheck == "match" || req.SampleCheck == "mismatch"
			} else if req.BodyCandidate {
				bodyCandidates++
			}
			hasSample = hasSample || req.SampleCheck != ""
		}
		if attachments != 0 {
			headers := fmt.Sprintf("HTTP attachment headers observed (%d)", attachments)
			if !comparedAttachment {
				headers += ", no linked byte comparison"
			}
			evidence = append(evidence, headers)
		}
		if bodyCandidates != 0 {
			evidence = append(evidence, fmt.Sprintf("HTTP body candidates without attachment headers (%d)", bodyCandidates))
		}
		if obs.SampleStreamsOpened != 0 {
			evidence = append(evidence, fmt.Sprintf("attachment streams opened by the probe (%d)", obs.SampleStreamsOpened))
		}
		if len(obs.Downloads) != 0 {
			evidence = append(evidence, "browser download event ("+strings.Join(obs.Downloads, ", ")+", local saving blocked)")
		} else if obs.Operation == "download" && attachments == 0 && bodyCandidates == 0 && !hasSample {
			evidence = append(evidence, "no browser download start observed")
		}
		if len(obs.Navigation) != 0 {
			evidence = append(evidence, "browser page activity ("+strings.ReplaceAll(strings.Join(obs.Navigation, ", "), "_", " ")+")")
		}
		if len(obs.PageNotices) != 0 {
			evidence = append(evidence, "visible page signals ("+strings.ReplaceAll(strings.Join(obs.PageNotices, ", "), "_", " ")+")")
		}
		if obs.ReportedResult != "" {
			evidence = append(evidence, "reported outcome ("+strings.ReplaceAll(obs.ReportedResult, "_", " ")+")")
		}
		if len(evidence) != 0 {
			fmt.Fprintf(&summary, "Observation %d. %s.\n", i+1, strings.Join(evidence, ", "))
		}
		if obs.Truncated {
			fmt.Fprintf(&summary, "Observation %d. Network evidence was truncated.\n", i+1)
		}
		if sample && (obs.Operation == "download" || hasSample) {
			_ = printSampleCheck(&summary, i+1, obs.networkReport)
		}
	}
	_, err := io.WriteString(out, summary.String())
	return err
}

const operationLabels = "list details search download upload rename move delete other"
const visibleResults = "browser preview client membership denied unknown"
const downloadOutcomeMenu = "Optional result 1 preview, 2 desktop app, 3 membership, 4 denied, 5 no visible change."

var (
	operations       = strings.Fields(operationLabels)
	results          = strings.Fields(visibleResults)
	downloadOutcomes = [...]string{"preview", "client", "membership", "denied", "no_visible_change"}
)

func observeOperations(ctx context.Context, in *bufio.Reader, out io.Writer, next string, mark func()) ([]operationReport, error) {
	observations := []operationReport{}
	focused := next == "download"
	guidance := "The observation window blocks saving downloads."
	if !focused {
		guidance = "Use files you own. For changes, use a disposable file. " + guidance + "\nOptional annotation example: delete denied."
	}
	if _, err := fmt.Fprintln(out, guidance); err != nil {
		return observations, err
	}
	for {
		observation, done, err := readObservation(ctx, in, out, len(observations)+1, next, focused)
		if focused && len(observations) != 0 && errors.Is(err, io.EOF) {
			return observations, nil
		}
		if done || err != nil {
			return observations, err
		}
		mark()
		observations = append(observations, observation)
		switch {
		case next == "upload" && observation.Operation == "upload":
			next = "download"
		case next == "download" && observation.Operation == "download" && !focused:
			next = ""
		}
	}
}

func waitForEnter(ctx context.Context, in *bufio.Reader, out io.Writer, prompt string) error {
	_, err := promptLine(ctx, in, out, prompt)
	return err
}

func readObservation(ctx context.Context, in *bufio.Reader, out io.Writer, number int, next string, focused bool) (operationReport, bool, error) {
	action := "Perform one action in the browser."
	menu := ""
	switch next {
	case "upload":
		action = "Upload the test ZIP and wait for it to appear."
	case "download":
		action = "Follow one download route to its outcome. Continue through any website steps it offers."
		menu = "\n" + downloadOutcomeMenu
	}
	ending := "Press Enter to record, or q then Enter to finish."
	if focused {
		ending = "Press Enter to mark this route, or q then Enter to finish."
	}
	prompt := fmt.Sprintf("Observation %d. %s %s%s", number, action, ending, menu)
	for {
		answer, err := promptLine(ctx, in, out, prompt)
		if err != nil {
			return operationReport{}, false, err
		}
		answer = strings.TrimSpace(answer)
		if answer == "q" {
			return operationReport{}, true, nil
		}
		if answer == "" {
			return operationReport{Operation: next}, false, nil
		}
		if next == "download" && len(answer) == 1 {
			index := int(answer[0]) - '1'
			if index >= 0 && index < len(downloadOutcomes) {
				return operationReport{Operation: next, ReportedResult: downloadOutcomes[index]}, false, nil
			}
		}
		fields := strings.Fields(strings.ToLower(answer))
		if len(fields) > 0 && len(fields) <= 2 && (!focused || fields[0] == "download") && slices.Contains(operations, fields[0]) {
			if len(fields) == 1 {
				return operationReport{Operation: fields[0]}, false, nil
			}
			if slices.Contains(results, fields[1]) {
				return operationReport{Operation: fields[0], ReportedResult: fields[1]}, false, nil
			}
		}
		prompt = ending + menu
		if !focused {
			prompt += "\nOperations [" + operationLabels + "]\nOptional result [" + visibleResults + "]"
		}
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
