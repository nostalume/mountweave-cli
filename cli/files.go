package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	core "github.com/nostalume/mountweave"
)

func fileAddress(operand string, listing bool) (core.Address, error) {
	raw, keyAddress := strings.CutPrefix(operand, "key:")
	if !keyAddress {
		return core.Path(operand), nil
	}
	encodedMount, key, ok := strings.Cut(raw, ":")
	mount, err := url.PathUnescape(encodedMount)
	if !ok || err != nil {
		return core.Address{}, core.Fail(core.InvalidPath, nil)
	}
	if listing {
		return core.Prefix(mount, key), nil
	}
	return core.Key(mount, key), nil
}

// These budgets bound discovery, including the native directory fallbacks of mv.
var directoryPlan = core.TreeOptions{MaxEntries: 10000, MaxPlanBytes: 8 << 20}

func (app *App) runFile(ctx context.Context, saved configSnapshot, a arguments, in io.Reader, out, diagnostics io.Writer) int {
	operands := a.operands
	if a.command == "put" {
		operands = operands[1:]
	} else if a.command == "get" {
		operands = operands[:1]
	}
	var addresses [2]core.Address
	for i, operand := range operands {
		addr, err := fileAddress(operand, a.command == "ls")
		if err != nil {
			return respond(a.json, out, diagnostics, nil, "", err)
		}
		if addr.Namespace() == core.KeyNamespace && (a.recursive || a.command == "mkdir" || a.command == "rmdir") {
			return respond(a.json, out, diagnostics, nil, "", core.Fail(core.UnsupportedOperation, nil))
		}
		addresses[i] = addr
	}
	c, err := app.openFor(ctx, saved, addresses[:len(operands)]...)
	if err != nil {
		return respond(a.json, out, diagnostics, nil, "", err)
	}
	if a.command == "ls" {
		return list(ctx, c, a, addresses[0], out, diagnostics)
	}
	result, err := fileOperation(ctx, c, a, addresses, in, out)
	err = errors.Join(err, closeRuntime(c))
	line := ""
	if !a.json {
		switch value := result.(type) {
		case core.Entry:
			line = fmt.Sprintf("%s\t%q\n", value.Kind, value.Address.Value())
		case fileReceipt:
			if a.command == "put" {
				switch value.Effect {
				case core.Applied.String():
					line = fmt.Sprintf("Uploaded %q.\n", a.operands[1])
				case core.Unchanged.String():
					line = fmt.Sprintf("Destination unchanged: %q.\n", a.operands[1])
				case core.Incomplete.String():
					line = fmt.Sprintf("Upload incomplete at %q.\n", a.operands[1])
				default:
					line = fmt.Sprintf("Could not verify whether %q was written.\n", a.operands[1])
				}
			}
		}
	}
	return respond(a.json, out, diagnostics, result, line, err)
}

func fileOperation(ctx context.Context, c *core.Core, a arguments, addr [2]core.Address, in io.Reader, out io.Writer) (any, error) {
	var write core.WriteOptions
	if a.replace {
		write.Policy = core.Publish()
	}
	switch a.command {
	case "stat":
		entry, err := c.Stat(ctx, localActor, addr[0])
		if err != nil {
			return nil, err
		}
		return entry, nil
	case "get":
		change, err := download(ctx, c, addr[0], a.operands[1], out)
		if a.operands[1] == "-" {
			return nil, err
		}
		return changeReceipt(change), err
	case "put":
		change, err := upload(ctx, c, addr[0], a.operands[0], in, a.size, write)
		return changeReceipt(change), err
	case "cp":
		if a.recursive {
			tree, err := c.CopyTree(ctx, localActor, addr[0], addr[1], directoryPlan)
			return treeReceipt(tree), err
		}
		change, err := c.Copy(ctx, localActor, addr[0], addr[1], core.CopyOptions{Target: write})
		return changeReceipt(change), err
	case "mv":
		move, err := c.Move(ctx, localActor, addr[0], addr[1], core.MoveOptions{Tree: directoryPlan})
		return moveReceipt(move), err
	case "mkdir":
		change, err := c.Mkdir(ctx, localActor, addr[0])
		return changeReceipt(change), err
	case "rm", "rmdir":
		if a.recursive {
			tree, err := c.RemoveTree(ctx, localActor, addr[0], directoryPlan)
			return treeReceipt(tree), err
		}
		target := core.RemoveNonDirectory
		if a.command == "rmdir" {
			target = core.RemoveEmptyDirectory
		} else if addr[0].Namespace() == core.KeyNamespace {
			target = "" // An exact object key is already a non-directory address.
		}
		change, err := c.Remove(ctx, localActor, addr[0], core.RemoveOptions{Target: target})
		return changeReceipt(change), err
	default:
		return nil, usage("Unknown file command.")
	}
}

type fileReceipt struct {
	Effect              string         `json:"effect,omitempty"`
	SourceEffect        string         `json:"source_effect,omitempty"`
	TargetEffect        string         `json:"target_effect,omitempty"`
	TargetRemovedEffect string         `json:"target_removed_effect,omitempty"`
	Items               *[]itemReceipt `json:"items,omitempty"`
}

type itemReceipt struct {
	Path      string              `json:"relative_path"`
	Side      core.ResultSide     `json:"side"`
	Phase     core.OperationPhase `json:"phase"`
	Effect    string              `json:"effect"`
	Error     string              `json:"error,omitempty"`
	ErrorSide core.ResultSide     `json:"error_side,omitempty"`
}

func changeReceipt(change core.Change) fileReceipt {
	return fileReceipt{Effect: change.Effect.String()}
}

func treeReceipt(tree core.TreeResult) fileReceipt {
	items := make([]itemReceipt, len(tree.Items))
	for i, item := range tree.Items {
		r := itemReceipt{Path: item.RelativePath, Side: item.Side, Phase: item.Phase, Effect: item.Change.Effect.String(), ErrorSide: item.ErrorSide}
		if p := publicError(item.Err); p != nil {
			r.Error = p.Code
		}
		items[i] = r
	}
	return fileReceipt{Effect: tree.Change.Effect.String(), Items: &items}
}

func moveReceipt(move core.MoveResult) fileReceipt {
	r := fileReceipt{SourceEffect: move.Source.Effect.String(), TargetEffect: move.Target.Effect.String()}
	if move.Tree != nil {
		r.Items = treeReceipt(*move.Tree).Items
	}
	if move.Removed != nil {
		removed := treeReceipt(*move.Removed)
		r.TargetRemovedEffect = removed.Effect
		if r.Items == nil {
			r.Items = removed.Items
		} else {
			items := append(*removed.Items, (*r.Items)...)
			r.Items = &items
		}
	}
	return r
}

func (r fileReceipt) write(w io.Writer, items bool) error {
	for _, effect := range []struct{ label, value string }{
		{"Effect", r.Effect}, {"Source effect", r.SourceEffect}, {"Target effect", r.TargetEffect}, {"Prior target removal", r.TargetRemovedEffect},
	} {
		if effect.value != "" {
			if _, err := fmt.Fprintf(w, "%s: %s\n", effect.label, effect.value); err != nil {
				return err
			}
		}
	}
	if items && r.Items != nil {
		for _, item := range *r.Items {
			if _, err := fmt.Fprintf(w, "%s %s %q: %s %s %s\n", item.Side, item.Phase, item.Path, item.Effect, item.Error, item.ErrorSide); err != nil {
				return err
			}
		}
	}
	return nil
}
