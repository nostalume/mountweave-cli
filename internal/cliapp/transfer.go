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
		if file, ok := out.(*os.File); ok {
			pipe, release, setupErr := pollablePipe(file)
			if setupErr != nil {
				return change, errors.Join(setupErr, content.Body.Close())
			}
			if release != nil {
				defer func() { err = errors.Join(err, release()) }()
			}
			out = pipe
		}
		if closer, ok := out.(io.Closer); ok {
			defer closeOnCancel(ctx, closer)()
		}
		err = copyDownload(outputBody{out}, content)
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
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
		return core.Fail(core.InvalidUpstreamResponse, io.ErrUnexpectedEOF)
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

func upload(ctx context.Context, c *core.Core, target core.Address, input string, in io.Reader, length *int64, write core.WriteOptions) (change core.Change, err error) {
	change.Effect = core.Unchanged
	if input != "-" {
		file, err := os.Open(input)
		if err != nil {
			return change, err
		}
		defer func() {
			closeErr := file.Close()
			if !errors.Is(closeErr, os.ErrClosed) {
				err = errors.Join(err, closeErr)
			}
		}()
		info, err := file.Stat()
		if err != nil {
			return change, err
		}
		if info.IsDir() {
			return change, core.Fail(core.NotFile, nil)
		}
		if info.Mode().IsRegular() {
			n := info.Size()
			length = &n
		}
		in = file
	}
	if input == "-" {
		if file, ok := in.(*os.File); ok {
			pipe, release, setupErr := pollablePipe(file)
			if setupErr != nil {
				return change, setupErr
			}
			if release != nil {
				defer func() { err = errors.Join(err, release()) }()
			}
			in = pipe
		}
	}
	if closer, ok := in.(io.Closer); ok {
		defer closeOnCancel(ctx, closer)()
	}
	change, err = c.Write(ctx, LocalActor, target, in, length, write)
	if err != nil {
		err = errors.Join(ctx.Err(), err)
	}
	return change, err
}

func closeOnCancel(ctx context.Context, closer io.Closer) func() {
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
