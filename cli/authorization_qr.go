package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	core "github.com/nostalume/mountweave"
	qrcode "github.com/skip2/go-qrcode"
)

// showAuthorizationQR keeps the temporary image available until the scan
// session ends. The terminal remains usable when an image viewer cannot start.
func showAuthorizationQR(ctx context.Context, out io.Writer, app, payload string) (func(), error) {
	qr, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		return nil, core.Fail(core.InvalidUpstreamResponse, nil)
	}
	if _, err := fmt.Fprintf(out, "Scan the QR code in the %s app and approve access.\n", app); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "mweave-authorization-*.png")
	if err == nil {
		name := file.Name()
		if err := errors.Join(qr.Write(384, file), file.Close()); err == nil {
			openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := launchOAuthBrowser(openCtx, name)
			cancel()
			if ctx.Err() != nil {
				_ = os.Remove(name)
				return nil, ctx.Err()
			}
			if err == nil {
				return func() { _ = os.Remove(name) }, nil
			}
		}
		_ = os.Remove(name)
	}
	_, err = fmt.Fprintf(out, "\x1b[30;107m%s\x1b[0m\n", qr.ToSmallString(true))
	return func() {}, err
}
