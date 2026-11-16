//go:build !windows

package cli

import (
	"context"
	"os/exec"
	"runtime"
)

func openBrowser(ctx context.Context, target string) error {
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	return exec.CommandContext(ctx, command, target).Run()
}
