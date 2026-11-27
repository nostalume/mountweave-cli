# Browser cloud checks

Run this command from the CLI repository with a compatible browser available.

```sh
go run ./cmd/mweave-browser-check https://drive.uc.cn/
```

On first use, sign in to the isolated window and close it. Later runs reopen
the saved session for the same HTTPS origin. Use `-signin` when the session
expires or you need another account. Use `-fresh` for a disposable session.

Use the website normally. Close the observation window or press Ctrl+C in
the terminal when finished. The probe saves an anonymous JSON report in
`mweave-browser-check/reports` under the user cache directory and prints
its path. Redirect stdout to write the JSON to another file.

## Inspect endpoints and JSON on this computer

```sh
go run ./cmd/mweave-browser-check -details site https://drive.uc.cn/
go run ./cmd/mweave-browser-check -details https://api.123278.com,https://api.123panapi.com,https://api.123panapi.cn https://yun.123pan.cn/
```

Use `site` for the entry origin, or enter the HTTPS origins that serve
the API. An origin list may also include `site`. When a scoped request is
observed, the report keeps its endpoint even if no response arrives.
`request_json` and `response_json` hold JSON objects or arrays from
Fetch and XHR, including error responses. Each body is limited to 16 KiB.
The capture states show whether data was captured, unavailable or over budget.
Login, token and session bodies are excluded. URL queries, headers and common
credential fields are omitted or redacted. Use the sample workflow below to
compare file bytes.

When the browser supplies a script stack for an eligible Fetch or XHR request,
`initiator` keeps up to four sanitized script locations from the selected
origins. Include the script's origin in `-details` when it differs from the
API origin. Line and column start at one. `request_offset_ms` is measured
from the earliest retained request in the same observation. Function names,
source code and script URL queries are omitted.

Detailed reports are saved under `mweave-browser-check-details` in the
user's cache directory. The terminal prints the report path. Detailed
reports can contain account and file metadata. Use the default report
for sharing.

Use the guided download check to compare website routes for the same file.

```sh
go run ./cmd/mweave-browser-check -download https://drive.uc.cn/
```

Use a file you own. Follow any menu or dialog the website offers to a visible
result, then press Enter to mark that route. For another route, return to the
same file and repeat. Enter `q` to save the report. A number in the prompt can
mark a route with its visible result, such as preview or a desktop app prompt.

Prompts go to stderr. The JSON report contains `opening` and ordered
`observations`. Guided download checks include a terminal summary. The tool listens continuously. Late responses
and redirect hops can follow their original request into its observation.
Request numbers are local to each observation. `unobserved_responses`
counts hops without matching response headers. `truncated` means some
earlier events were discarded.

- `operation` and `reported_result` describe what the user reported or what
  the guided workflow prompted them to do.
- `navigation` includes page changes in the original tab, same-document route
  changes, and window activity. `window_open_requested` means the browser
  reported a request to open a window. `new_tab` means a new page target was
  linked to the original page by its opener. `new_tab_attached` means the
  probe started listening in that tab. Its first navigation may precede
  attachment. `new_tab_unobserved` means the tab could not be attached or the
  limit of eight related tabs was reached. `iframe_document` marks a frame
  navigation visible to the observer. `external_protocol_requested` records a
  navigation request, which can occur without a desktop app launching.
  Unrelated tabs and separate cross-site frame targets are outside the capture.
- `requests` shows anonymous HTTP evidence. `transfer` describes how the last
  request hop ended. `related_tab` is a run-local number for an opener-linked
  tab. `body_candidate` indicates an attachment, a range
  response or another possible file body. `content_disposition` records
  `attachment`, `inline`, or `other` when present, without header parameters.
  The terminal lists body candidates without attachment headers separately from
  attachment headers, browser download events and sample byte comparison.
  The probe also checks attachment streams in tabs opened directly by the original tab after sample checks start.
- `sample_streams_opened` counts attachments whose body stream the probe
  obtained. `sample_check` reports whether the bytes were verified.
- `page_notices` contains recognized status messages and download choices in
  visible dialogs or menus. Fixed categories such as `rename_success` or
  `ordinary_download_option` keep notice text in the browser. A visible choice
  is evidence of an option. Persistent file effects require a fresh directory
  or recycle-bin receipt for the same file.
- Provider-enabled development builds include `file_effects` for single-file
  changes on `f.115.com`. The Mountweave Browser module pairs an accepted
  operation with fresh directory responses from the same
  page session. `rename_confirmed` verifies the new name. `move_confirmed`
  verifies the same file in the destination directory. `delete_source_confirmed`
  verifies absence from a complete, unfiltered source listing. Recycle-bin
  rows use a separate ID and lack a link to the original file ID. An accepted
  operation with insufficient readback evidence is recorded as `*_unconfirmed`.
  Only these fixed results are saved.
- `downloads` lists browser download states (`started`, `canceled`, or
  `completed`). If an event cannot be linked to one request, it stays in the
  observation where it arrived.
  `download_saving: "blocked"` describes the tool's local saving setting.
  An attachment response can appear without a browser download event.

The report omits captured URLs, credentials, filenames and response bodies.

## Check a download with a generated sample

Use `-sample` to generate a small test ZIP.

```sh
go run ./cmd/mweave-browser-check -sample https://drive.uc.cn/
```

Upload the generated ZIP. Sample checks already run when the site opens.
If Download opens a new page, follow that tab through its final Download button.
Keep the observation running until the final action reaches an outcome.
Use Enter to mark steps. Remove the uploaded copy before entering `q`.
The tool deletes the local ZIP on exit.

To verify a test ZIP already uploaded, reuse the corresponding local sample.

```sh
go run ./cmd/mweave-browser-check -sample-file ./test-sample.zip https://drive.uc.cn/
```

Follow a download route on that ZIP, mark it, and enter `q` when done. The tool
finishes checking any attachment it has already started reading. The result distinguishes a complete
byte match, a different body, and unavailable evidence. A verified sample
whose anonymous request was lost is recorded in `unlinked_sample_checks`.

During the sample download step, the probe checks attachments in memory on the
main page and tabs opened directly by it, including an immediate redirect.
It reads a final full response matching the exact filename. Any declared
length must match. The read verifies the exact byte count, EOF and digest
even when the length is absent. Local saving stays blocked, so a verified
sample can still show a canceled transfer.

At the end, the terminal shows the sample result and anonymous request
numbers. The JSON includes `sample_check` for eligible full `GET 200` responses.
The browser cache and check time are bounded. Older response bodies may be
evicted. A `206` contributes range evidence.

- `match` means the response bytes match the sample. Multiple responses can match.
- `mismatch` means the bytes differ.
- `headers_only` records an attachment response without a linked byte check.
- `stream_unavailable` means the browser did not supply the selected attachment stream.
- `read_incomplete` means an opened stream did not prove the exact length and EOF.
- `unavailable` covers other bodies the probe could not verify, including evicted
  bodies and browser-decoded text.
- `not_checked` means the check reached the per-observation response limit or the time limit.

File names, digests, and bodies stay out of the report. Matching bytes alone
cannot identify the account or file.

## Record file changes

```sh
go run ./cmd/mweave-browser-check -changes https://drive.uc.cn/
```

The same isolated window handles sign-in and the check. Press Enter in the
terminal when your files appear. Capture starts then. Use the two generated
ZIPs in a new test folder and check the resulting directory or recycle bin.
Leave the window open and press Enter again in the terminal to save, close the
window, and finish this site. For a folder upload check, use the generated
folder with the first ZIP's name. The private report lists generated ZIP names
so their receipts can be found later. The probe removes the local samples on exit.
You can use `-details` to name additional HTTPS API origins and `-report-dir`
to put the results in a private directory for a batch. Use `-restore-session`
when the site relies on retained browser session cookies.

Detailed reports include small file-operation forms and an allowlisted
operation from the request query. A task number links receipts with the same
task ID on the same origin within a report. The task state and task error
code are separate from HTTP and business codes. Nested task receipts are
linked when their identifiers agree. Refresh or
reopen the folder to confirm the effect on the original file. Detailed
reports can contain file metadata and stay on this computer.

## Verify the effect

Use a disposable file you own for changes. Observe the starting state, perform
the action, then refresh or reopen and check the same file. The report
preserves the observed network activity for that session.

- For deletion, check both the original folder and the recycle bin. A request
  accepted for processing can leave the file present.
- For a rename or move, look for the same file at the new location and check
  the old one. Compare stable file identifiers locally when available.
- For upload, read the file back and compare it with the original.
- For download, use the generated sample above. If the webpage does not
  expose suitable response bytes, compare the actual download in your normal
  browser.

## Closed fixtures

From the CLI checkout, run the website fixtures with the published Driver
modules and `GOWORK=off`.

```sh
go test -mod=readonly -count=1 -v -run '^Test(AliPanSharedWebsiteGrant|LanZouWebsiteReceipt|UCWebsiteTransfer|QuarkCDPReceipt|WebsiteMountTransaction)$' ./internal/stock
go test -mod=readonly ./cmd/mweave-browser-check
go test -count=1 -v github.com/nostalume/mountweave/drivers/quark github.com/nostalume/mountweave/drivers/mobilepan
```

Fixtures that launch a browser report a skip when no supported browser is available.
