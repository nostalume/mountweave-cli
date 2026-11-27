package cliapp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	core "github.com/nostalume/mountweave"
)

func download(ctx context.Context, c *core.Core, source core.Address, dest string, out io.Writer) (change core.Change, err error) {
	change.Effect = core.Unchanged
	if dest != "-" {
		if _, err := os.Lstat(dest); err == nil {
			return change, core.Fail(core.AlreadyExists, nil)
		} else if !errors.Is(err, os.ErrNotExist) {
			return change, err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	content, err := c.Open(ctx, LocalActor, source, core.OpenOptions{})
	if err != nil {
		return change, err
	}
	if dest == "-" {
		out, finish, setupErr := prepareStream(ctx, out, true)
		if setupErr != nil {
			return change, errors.Join(setupErr, content.Body.Close())
		}
		defer finish(&err)
		if err = copyDownload(outputBody{out}, content); err != nil {
			if canceled := ctx.Err(); canceled != nil {
				err = canceled
			}
			cancel()
		}
		return change, errors.Join(err, content.Body.Close())
	}

	stage, err := os.CreateTemp(filepath.Dir(dest), ".mweave-download-*")
	if err != nil {
		cancel()
		return change, errors.Join(err, content.Body.Close())
	}
	defer func() { err = errors.Join(err, os.Remove(stage.Name())) }()
	err = copyDownload(stage, content)
	if err != nil {
		cancel()
	}
	if err := errors.Join(err, content.Body.Close(), stage.Close()); err != nil {
		return change, err
	}
	if err := ctx.Err(); err != nil {
		return change, err
	}
	// The same-directory link is the exclusive publication point.
	if err := os.Link(stage.Name(), dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			return change, core.Fail(core.AlreadyExists, nil)
		}
		return change, &Problem{Code: "io_error", Message: "Could not publish the completed local download.", NextAction: "Check the destination or use get ADDRESS - to manage the output yourself."}
	}
	change.Effect = core.Applied
	return change, ctx.Err()
}

func copyDownload(w io.Writer, content core.Content) error {
	n, err := io.Copy(w, content.Body)
	if err == nil && content.Length != nil && n != *content.Length {
		err = io.ErrUnexpectedEOF
	}
	if err == io.ErrUnexpectedEOF {
		return core.Fail(core.InvalidUpstreamResponse, err)
	}
	return err
}

type outputBody struct{ io.Writer }

func (w outputBody) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err != nil || n != len(p) {
		return n, &Problem{Code: "output_error", Message: "The download could not be written."}
	}
	return n, nil
}

func upload(ctx context.Context, c *core.Core, target core.Address, inDirectory bool, input string, in io.Reader, length *int64, write core.WriteOptions) (destination core.Address, change core.Change, err error) {
	change.Effect = core.Unchanged
	if input != "-" {
		file, err := os.Open(input)
		if err != nil {
			return destination, change, err
		}
		defer func() {
			if closeErr := file.Close(); !errors.Is(closeErr, os.ErrClosed) {
				err = errors.Join(err, closeErr)
			}
		}()
		info, err := file.Stat()
		if err != nil {
			return destination, change, err
		}
		if info.IsDir() {
			return destination, change, core.Fail(core.NotFile, nil)
		}
		if info.Mode().IsRegular() {
			n := info.Size()
			length = &n
		}
		in = file
	}
	in, finish, err := prepareStream(ctx, in, input == "-")
	if err != nil {
		return destination, change, err
	}
	defer finish(&err)
	if inDirectory {
		destination, change, err = c.WriteInto(ctx, LocalActor, target, filepath.Base(input), in, length)
	} else {
		change, err = c.Write(ctx, LocalActor, target, in, length, write)
	}
	if err != nil {
		err = errors.Join(ctx.Err(), err)
	}
	return destination, change, err
}

// prepareStream joins cancellation before restoring any borrowed pipe flags.
func prepareStream[T any](ctx context.Context, stream T, poll bool) (T, func(*error), error) {
	var release func() error
	if file, ok := any(stream).(*os.File); ok && poll {
		pipe, done, err := pollablePipe(file)
		if err != nil {
			return stream, nil, err
		}
		stream, release = any(pipe).(T), done
	}
	stop := closeOnCancel(ctx, stream)
	return stream, func(err *error) {
		stop()
		if release != nil {
			if restore := release(); restore != nil && err != nil {
				*err = errors.Join(*err, restore)
			}
		}
	}, nil
}

func closeOnCancel(ctx context.Context, stream any) func() {
	closer, ok := stream.(io.Closer)
	if !ok {
		return func() {}
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = closer.Close()
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}
