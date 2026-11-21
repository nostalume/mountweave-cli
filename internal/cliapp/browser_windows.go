package cliapp

import (
	"context"

	"golang.org/x/sys/windows"
)

func openBrowser(ctx context.Context, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, nil, path, nil, nil, windows.SW_SHOWNORMAL)
}
