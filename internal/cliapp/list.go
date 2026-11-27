package cliapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	core "github.com/nostalume/mountweave"
)

func (app *App) list(ctx context.Context, c *core.Core, a arguments, address core.Address, out, diagnostics io.Writer, project fileErrorProjection, release func(string) error) int {
	out, finish, err := prepareStream(ctx, out, true)
	if err != nil {
		return app.listOutputFailed(diagnostics, release(""))
	}
	defer finish(nil)

	buffer := bufio.NewWriter(out)
	encoder := json.NewEncoder(buffer)
	if a.json {
		buffer.WriteString(`{"result":{"items":[`)
	}
	pageSize := core.DefaultPage
	if a.limit > 0 {
		pageSize = min(a.limit, pageSize)
	}
	query := core.Children(address)
	if address.IsPrefix() {
		query = core.Keys(address.Space(), address.Value(), "")
	}
	req := core.ListRequest{Query: query, Limit: pageSize, CursorTTL: time.Minute}
	state, count := "complete", 0
	var outputErr error
	for {
		page, pageErr := c.List(ctx, LocalActor, req)
		if pageErr != nil {
			err = project(pageErr, "", "")
			break
		}
		req.Cursor = ""
		if page.NextCursor != nil {
			req.Cursor = *page.NextCursor
		}
		items := page.Items
		if a.limit > 0 && len(items) > a.limit-count {
			items = items[:a.limit-count]
			state = "limited"
		}
		for _, entry := range items {
			if a.json {
				if count > 0 {
					buffer.WriteByte(',')
				}
				outputErr = encoder.Encode(entry)
			} else {
				name := displayText(entry.Name)
				if entry.Kind == core.Directory {
					name += "/"
				}
				_, outputErr = fmt.Fprintln(buffer, name)
			}
			if outputErr != nil {
				break
			}
			count++
		}
		if outputErr == nil {
			outputErr = buffer.Flush()
		}
		if outputErr != nil || req.Cursor == "" {
			break
		}
		if a.limit > 0 && count == a.limit {
			state = "limited"
			break
		}
	}
	retirement := release(req.Cursor)
	if outputErr != nil {
		return app.listOutputFailed(diagnostics, retirement)
	}
	err = errors.Join(err, retirement)
	p := AppProblem(app, err)
	if a.json {
		if err != nil {
			state = "failed"
		}
		body, encodeErr := json.Marshal(p)
		_, writeErr := fmt.Fprintf(buffer, `],"state":%q},"error":%s}`+"\n", state, body)
		if encodeErr != nil || writeErr != nil || buffer.Flush() != nil {
			return app.listOutputFailed(diagnostics, retirement)
		}
	} else if p != nil {
		printProblem(diagnostics, p)
	}
	return status(p)
}

func (app *App) listOutputFailed(diagnostics io.Writer, retirement error) int {
	outputFailure(diagnostics)
	if retirement != nil {
		fmt.Fprintf(diagnostics, "shutdown_failed: Mount retirement. %s\n", AppProblem(app, retirement).Message)
	}
	return 1
}
