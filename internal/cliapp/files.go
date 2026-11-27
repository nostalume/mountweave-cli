package cliapp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	core "github.com/nostalume/mountweave"
)

// These budgets bound discovery, including the native directory fallbacks of mv.
var directoryPlan = core.TreeOptions{MaxEntries: 10000, MaxPlanBytes: 8 << 20}

func (app *App) runFile(ctx context.Context, saved ConfigSnapshot, a arguments, in io.Reader, out, diagnostics io.Writer, shared *fileRuntime) int {
	var addresses [2]core.Address
	var localFile string
	inDirectory := false
	targets, listing := addresses[:0], a.command.name == "ls"
	if listing && len(a.operands) == 0 {
		targets = append(targets, core.Path("/"))
	}
	for i, operand := range a.operands {
		role := a.command.operands[i]
		if role == localFileOperand {
			localFile = operand
			continue
		}
		isDirectoryTarget := i == 1 && directoryTarget(operand) &&
			(a.command.name == "mv" || a.command.name == "put" || a.command.name == "cp")
		inDirectory = inDirectory || isDirectoryTarget
		addr, err := fileAddress(operand, listing, a.recursive || role == treePathOperand || isDirectoryTarget)
		if err != nil {
			return app.respond(a.json, out, diagnostics, nil, "", err)
		}
		targets = append(targets, addr)
	}
	if inDirectory && (a.replace || localFile == "-") {
		return app.respond(a.json, out, diagnostics, nil, "", usage("Standard input and --replace require a full destination path."))
	}
	virtual := listing || a.command.name == "stat"
	var c *core.Core
	var err error
	finish := func(cursor string) error {
		if shared == nil {
			return CloseRuntime(c)
		}
		if cursor == "" {
			return nil
		}
		cleanup, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err := c.ReleaseCursor(cleanup, LocalActor, cursor)
		if core.CodeOf(err) == core.CursorUnavailable {
			return nil
		}
		return err
	}
	if shared == nil {
		c, err = app.openFor(ctx, saved, virtual, targets...)
	} else {
		c = shared.core
		err = app.installFor(ctx, saved, shared, virtual, targets...)
	}
	if err != nil {
		return app.respond(a.json, out, diagnostics, nil, "", err)
	}
	project := func(err error, side core.ResultSide, relative string) error {
		return app.fileError(saved, targets, err, side, relative)
	}
	if listing {
		return app.list(ctx, c, a, addresses[0], out, diagnostics, project, finish)
	}
	result, err := app.fileOperation(ctx, c, a, addresses, inDirectory, localFile, in, out, project)
	err = errors.Join(project(err, "", ""), finish(""))
	line := ""
	if !a.json {
		switch value := result.(type) {
		case core.Entry:
			line = entryText(value)
		case fileReceipt:
			if a.command.name != "put" {
				break
			}
			if inDirectory && value.Destination == "" {
				break
			}
			target := displayText(cmp.Or(value.Destination, addressText(addresses[0])))
			switch value.Effect {
			case core.Applied.String():
				line = fmt.Sprintf("Uploaded %s.\n", target)
			case core.Unchanged.String():
				line = fmt.Sprintf("Destination unchanged: %s.\n", target)
			case core.Incomplete.String():
				line = fmt.Sprintf("Upload incomplete at %s.\n", target)
			default:
				line = fmt.Sprintf("Could not verify whether %s was written.\n", target)
			}
		}
	}
	return app.respond(a.json, out, diagnostics, result, line, err)
}

type fileErrorProjection func(error, core.ResultSide, string) error

// Recovery can name a mount only when the failing operation identifies it.
func (app *App) fileError(saved ConfigSnapshot, targets []core.Address, err error, side core.ResultSide, relative string) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var known *Problem
	if errors.As(err, &known) && !known.Reconnect {
		return err
	}
	if len(targets) == 2 {
		switch operationSide(err, side) {
		case core.SourceSide:
			targets = targets[:1]
		case core.TargetSide:
			targets = targets[1:]
		}
	}
	var recovery *Problem
	var mount MountRecord
	ambiguous := false
	for _, candidate := range saved.Value.Mounts {
		for _, target := range targets {
			if target.Namespace() == core.TreeNamespace && relative != "" {
				target = core.Path(path.Join(target.Value(), relative))
			}
			if !mountIntersects(candidate.At, target) {
				continue
			}
			p := known
			if p == nil {
				p = app.mountRecoveryProblem(candidate, err)
			}
			if p == nil {
				break
			}
			if recovery == nil {
				recovery, mount = p, candidate
			} else if *recovery != *p {
				return err
			} else {
				ambiguous = true
			}
			break
		}
	}
	if recovery == nil {
		return err
	}
	if !ambiguous {
		p := *recovery
		p.addMountRecovery(saved.Path, mount)
		recovery = &p
	}
	return errors.Join(recovery, err)
}

func operationSide(err error, fallback core.ResultSide) core.ResultSide {
	for err != nil {
		switch failure := err.(type) {
		case *core.OperationError:
			return failure.Side
		case interface{ Unwrap() []error }:
			children := failure.Unwrap()
			if len(children) != 1 {
				return ""
			}
			err = children[0]
		default:
			err = errors.Unwrap(err)
		}
	}
	return fallback
}

func (app *App) fileOperation(ctx context.Context, c *core.Core, a arguments, addr [2]core.Address, inDirectory bool, localFile string, in io.Reader, out io.Writer, project fileErrorProjection) (any, error) {
	var write core.WriteOptions
	if a.replace {
		write.Policy = core.Publish()
	}
	var change core.Change
	var err error
	switch a.command.name {
	case "stat":
		entry, err := c.Stat(ctx, LocalActor, addr[0])
		if err != nil {
			return nil, err
		}
		return entry, nil
	case "get":
		change, err = download(ctx, c, addr[0], localFile, out)
		if localFile == "-" {
			return nil, err
		}
	case "put":
		destination, uploaded, err := upload(ctx, c, addr[0], inDirectory, localFile, in, a.size, write)
		receipt := changeReceipt(uploaded)
		receipt.Destination = addressText(destination)
		return receipt, err
	case "cp":
		if inDirectory {
			if a.recursive {
				destination, tree, err := c.CopyTreeInto(ctx, LocalActor, addr[0], addr[1], directoryPlan)
				receipt := app.treeReceipt(tree, project)
				receipt.Destination = addressText(destination)
				return receipt, err
			}
			destination, copied, err := c.CopyInto(ctx, LocalActor, addr[0], addr[1])
			receipt := changeReceipt(copied)
			receipt.Destination = addressText(destination)
			return receipt, err
		}
		if a.recursive {
			tree, err := c.CopyTree(ctx, LocalActor, addr[0], addr[1], directoryPlan)
			return app.treeReceipt(tree, project), err
		}
		change, err = c.Copy(ctx, LocalActor, addr[0], addr[1], core.CopyOptions{Target: write})
	case "mv":
		if inDirectory {
			move, err := c.MoveInto(ctx, LocalActor, addr[0], addr[1], directoryPlan)
			return app.moveReceipt(move, project), err
		}
		move, err := c.Move(ctx, LocalActor, addr[0], addr[1], core.MoveOptions{Tree: directoryPlan})
		return app.moveReceipt(move, project), err
	case "mkdir":
		change, err = c.Mkdir(ctx, LocalActor, addr[0])
	case "trash", "remove", "rmdir":
		disposition := core.RemovePermanent
		if a.command.name == "trash" {
			disposition = core.RemoveTrash
		}
		target := core.RemoveNonDirectory
		if a.command.name == "rmdir" {
			target = core.RemoveEmptyDirectory
		} else if addr[0].Namespace() == core.KeyNamespace {
			target = "" // An exact object key is already a non-directory address.
		}
		change, err = c.Remove(ctx, LocalActor, addr[0], core.RemoveOptions{Target: target, Disposition: disposition})
	default:
		return nil, usage("Unknown file command.")
	}
	return changeReceipt(change), err
}

type fileReceipt struct {
	Destination         string         `json:"destination,omitempty"`
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

func (app *App) treeReceipt(tree core.TreeResult, project fileErrorProjection) fileReceipt {
	items := make([]itemReceipt, len(tree.Items))
	for i, item := range tree.Items {
		r := itemReceipt{Path: item.RelativePath, Side: item.Side, Phase: item.Phase, Effect: item.Change.Effect.String(), ErrorSide: item.ErrorSide}
		if item.Err != nil {
			r.Error = AppProblem(app, project(item.Err, cmp.Or(item.ErrorSide, item.Side), item.RelativePath)).Code
		}
		items[i] = r
	}
	return fileReceipt{Effect: tree.Change.Effect.String(), Items: &items}
}

func (app *App) moveReceipt(move core.MoveResult, project fileErrorProjection) fileReceipt {
	r := fileReceipt{SourceEffect: move.Source.Effect.String(), TargetEffect: move.Target.Effect.String()}
	if move.Destination.Value() != "" {
		r.Destination = addressText(move.Destination)
	}
	if move.Tree != nil {
		r.Items = app.treeReceipt(*move.Tree, project).Items
	}
	if move.Removed != nil {
		removed := app.treeReceipt(*move.Removed, project)
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
	if r.Destination != "" {
		if _, err := fmt.Fprintf(w, "Destination: %s\n", displayText(r.Destination)); err != nil {
			return err
		}
	}
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
