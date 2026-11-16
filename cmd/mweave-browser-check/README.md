# Browser cloud checks

Run this command from the CLI repository with a compatible browser available.

```sh
go run ./cmd/mweave-browser-check https://drive.uc.cn/
```

Sign in to the isolated window, close it, and follow the terminal prompts to
demonstrate listing and downloading. Use an existing small file. Add
`-stage list`, `-stage full`, or `-stage range` before the URL to focus on one
operation after reopening. The range stage observes the site's own partial
request when available.

Prompts go to stderr. The JSON on stdout groups request and response evidence
by stage and uses anonymous identifiers within each stage. It records HTTP
statuses, including refusals, redirects, attachment signals, and the presence
of range headers. The `unobserved_responses` count identifies recorded request
hops with no matching HTTP response in the observation. A partial response is
a lead for the source fixture. Account identity, the selected file, and actual
body bytes still require source-specific assertions.

The observation window blocks saving downloads. The temporary browser profile
is removed after closing. The report contains no request URLs, cookies,
authorization headers, or body contents.

Run the existing closed acceptance tests to verify implemented source behavior.

```sh
go test -count=1 -v -run '^TestWebsiteRead' ./cli
```
