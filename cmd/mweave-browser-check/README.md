# Browser cloud checks

Run this command from the CLI repository with a compatible browser available.

```sh
go run ./cmd/mweave-browser-check https://drive.uc.cn/
```

On first use, sign in to the isolated window and close it. Later runs reopen
the saved session for the same HTTPS origin. Use `-signin` when the session
expires or you need another account. Use `-fresh` for a disposable session.

Wait for the terminal prompt, perform one action in the website, then press
Enter to keep that observation and start the next one. Type `q` and press
Enter to finish. Add a label such as `list` or `delete denied` before pressing
Enter when it helps explain the report. Range evidence is recorded as part of
the HTTP requests.

Prompts go to stderr. The JSON on stdout has one `opening` record and an
ordered `observations` array. Each observation contains anonymous HTTP
`requests`, plus `operation` and `reported_result` when supplied. Identifiers are
local to each record. `unobserved_responses` counts hops without a matching
response. `truncated` means earlier events were discarded.

`transfer` records a browser-observed finish, failure or cancellation on the
last request hop. A failed load before headers may have no HTTP status. An
omitted transfer leaves the outcome open at the observation boundary.

`content_kind` groups response types. Look for `body_candidate` when locating
file data. An attachment or a response to a range request can provide a clue
even if saving was canceled. A completed binary response can also qualify.

The report contains no request URLs, credentials or response bodies.

## Check a download with a generated sample

Use `-sample` to generate a small test ZIP.

```sh
go run ./cmd/mweave-browser-check -sample https://drive.uc.cn/
```

Upload the ZIP shown in the terminal and press Enter. In the website, click
Download for that file, then press Enter again. The saved session is reused
and local download saving stays disabled.

Quit with `q` to clean up the local ZIP. Delete the uploaded copy from the
website when finished.

After each mark, the terminal shows the sample result and anonymous request
numbers. The JSON includes the same finding as `sample_check`. The tool checks
the final hop of eligible full `GET 200` responses, starting with the most
recent. A `206` remains range evidence.

- `match` means the response bytes match the sample. Multiple responses can match.
- `mismatch` means the bytes differ.
- `unavailable` means the response did not finish or the browser could not supply
  byte-exact data. This includes browser-decoded text and evicted bodies.
- `not_checked` means the action reached the four-response or time limit.

File names, digests, and bodies stay out of the report. Matching bytes alone
cannot identify the account or file.

## Verify the effect

Use a disposable file you own for changes. Observe the starting state, perform
the action, then refresh or reopen and check the same file. Mark those steps
separately when you need the HTTP evidence for each one.

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
