package cliapp

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	core "github.com/nostalume/mountweave"
)

const shutdownTimeout = 5 * time.Second

type EndpointInfo struct {
	Protocol string `json:"protocol"`
	Listen   string `json:"listen"`
	Root     string `json:"root"`
	Access   string `json:"access"`
	TLS      bool   `json:"tls"`
}

type exportServer interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
	Close() error
}

type TLSExport struct{ *http.Server }

func (s TLSExport) Serve(l net.Listener) error { return s.ServeTLS(l, "", "") }

type endpoint struct {
	info     EndpointInfo
	Server   exportServer
	Listener net.Listener
	close    func(context.Context) error
}

type ExportRuntime struct {
	app       *App
	Core      *core.Core
	Endpoints []*endpoint
	cancel    context.CancelFunc
}

func (app *App) runServe(ctx context.Context, saved ConfigSnapshot, machine bool, out, diagnostics io.Writer) int {
	runtime, err := PrepareExports(app, ctx, saved, diagnostics)
	if err == nil {
		err = ctx.Err()
	}

	var code int
	var failure error
	started := false
	if err == nil {
		code = runtime.ready(ctx, machine, out, diagnostics)
		started = code == 0
		if started {
			failure = runtime.Run(ctx)
		}
	}
	cleanup := runtime.Shutdown(started, shutdownTimeout)
	if err != nil && !(errors.Is(err, context.Canceled) && ctx.Err() != nil) {
		code = app.respond(machine, out, diagnostics, nil, "", err)
	}
	if failure != nil {
		printProblem(diagnostics, AppProblem(app, failure))
	}
	if cleanup != nil {
		fmt.Fprintf(diagnostics, "shutdown_failed: %v\n", cleanup)
	}
	if failure != nil || cleanup != nil {
		return max(code, 1)
	}
	return code
}

func PrepareExports(app *App, ctx context.Context, saved ConfigSnapshot, diagnostics io.Writer) (runtime ExportRuntime, err error) {
	runtime.app = app
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	logger := log.New(diagnostics, "", 0)
	if len(saved.Value.Exports) == 0 {
		return runtime, InvalidConfig("No protocol exports are configured. Use mweave export add.")
	}
	if err := CheckStorageLocations(saved, true); err != nil {
		return runtime, err
	}
	scopes := make(map[core.Principal]exportScope, len(saved.Value.Exports))
	runtime.Core, err = core.New(core.Options{MaxCursors: 128, Authorize: func(_ context.Context, p core.Principal, a core.Action, addr core.Address) error {
		if !scopes[p].allows(a, addr) {
			return os.ErrPermission
		}
		return nil
	}})
	if err != nil {
		return runtime, err
	}

	// The stop signal begins a drain. Requests keep their own contexts until
	// the grace period ends or their client cancels.
	requests, cancel := context.WithCancel(context.WithoutCancel(ctx))
	runtime.cancel = cancel
	principals := make([]core.Principal, len(saved.Value.Exports))
	for i, e := range saved.Value.Exports {
		if err := e.checkCredential(saved); err != nil {
			return runtime, app.serveProblem(err, "Export at %q", e.Listen)
		}
		p := core.Principal(e.Listen)
		if e.S3 != nil && e.S3.Multipart != nil {
			p, err = s3MultipartIdentity(saved, e)
			if err != nil {
				return runtime, app.serveProblem(err, "Export at %q", e.Listen)
			}
		}
		principals[i], scopes[p] = p, e.scope()
	}

	ctx = withMountBindings(ctx)
	for _, m := range saved.Value.Mounts {
		if slices.ContainsFunc(saved.Value.Exports, func(e Export) bool { return e.containsMount(m.At) }) {
			if err := app.install(ctx, runtime.Core, saved, m); err != nil {
				return runtime, app.mountFailure(err, saved, m)
			}
		}
	}
	for i, e := range saved.Value.Exports {
		if e.Protocol == "s3" {
			// Resolve the native key binding without opening a cursor or reading storage.
			if _, err := runtime.Core.Capture(ctx, core.AddressAccess{Address: core.Prefix(e.Root, "")}); err != nil {
				if core.CodeOf(err) == core.UnsupportedOperation {
					err = InvalidConfig("S3 requires a mount with native object keys.")
				}
				return runtime, app.serveProblem(err, "Export root %q at %q", e.Root, e.Listen)
			}
			continue
		}
		entry, err := runtime.Core.Stat(ctx, principals[i], core.Path(e.Root))
		if err == nil && (entry.Kind != core.Directory || entry.Virtual) {
			err = core.Fail(core.NotDirectory, nil)
		}
		if err != nil {
			err = app.fileError(saved, []core.Address{core.Path(e.Root)}, err, "", "")
			return runtime, app.serveProblem(err, "Export root %q at %q", e.Root, e.Listen)
		}
	}

	for i, e := range saved.Value.Exports {
		if err := ctx.Err(); err != nil {
			return runtime, err
		}
		item, err := prepareEndpoint(ctx, requests, saved, runtime.Core, e, principals[i], logger)
		if err != nil {
			return runtime, app.serveProblem(err, "Export at %q", e.Listen)
		}
		runtime.Endpoints = append(runtime.Endpoints, item)
	}
	for _, item := range runtime.Endpoints {
		if err := ctx.Err(); err != nil {
			return runtime, err
		}
		var network net.ListenConfig
		if !item.info.TLS && item.info.Protocol != "sftp" {
			network.Control = requireLoopback
		}
		l, err := network.Listen(ctx, "tcp", item.info.Listen)
		if err != nil {
			if ctx.Err() != nil {
				return runtime, ctx.Err()
			}
			var configErr *Problem
			if !errors.As(err, &configErr) {
				err = &Problem{Code: "listener_failed", Message: "The address could not be bound.", NextAction: "Check whether the address is available."}
			}
			return runtime, app.serveProblem(err, "Listener at %q", item.info.Listen)
		}
		item.Listener = l
		item.info.Listen = l.Addr().String()
	}
	return runtime, nil
}

// ListenConfig supplies the resolved address before the socket is bound.
func requireLoopback(_ string, address string, _ syscall.RawConn) error {
	host, _, _ := net.SplitHostPort(address)
	host, _, _ = strings.Cut(host, "%")
	if !net.ParseIP(host).IsLoopback() {
		return InvalidConfig("Cleartext must bind to a loopback address.")
	}
	return nil
}

func prepareEndpoint(startup, ctx context.Context, saved ConfigSnapshot, c *core.Core, e Export, p core.Principal, logger *log.Logger) (*endpoint, error) {
	base := filepath.Dir(saved.Path)
	item := &endpoint{info: EndpointInfo{e.Protocol, e.Listen, e.Root, cmp.Or(e.Access, "read"), e.TLS != nil}}
	if e.Protocol == "sftp" {
		return item.withTCP(prepareSFTPExport(ctx, c, e, p, base))
	}
	secret, err := LoadCredential(e.Auth.Credential, base)
	if err != nil {
		return nil, err
	}
	var tlsConfig *tls.Config
	if e.TLS != nil {
		cert, err := tls.LoadX509KeyPair(e.TLS.paths(base))
		if err != nil {
			return nil, &Problem{Code: "invalid_config", Message: "The TLS certificate and key could not be loaded.", NextAction: "Check the certificate and key files."}
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}
	if e.Protocol == "ftp" {
		return item.withTCP(prepareFTPExport(ctx, c, e, p, secret, tlsConfig))
	}
	var handler http.Handler
	if e.Protocol == "s3" {
		// Pass the exact signed request target to S3 without ServeMux path cleaning.
		handler, item.close, err = s3ExportHandler(startup, ctx, saved, c, e, p, secret, logger)
	} else {
		handler, item.close, err = directoryHTTPHandler(c, e, p, secret)
	}
	if err != nil {
		return nil, err
	}
	server := &http.Server{
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		// net/http can log peer input and panic values. Emit only host-owned errors.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	item.Server = server
	if tlsConfig != nil {
		item.Server = TLSExport{server}
	}
	return item, nil
}

func (e *endpoint) withTCP(server exportServer, err error) (*endpoint, error) {
	if err != nil {
		return nil, err
	}
	e.Server, e.close = server, server.Shutdown
	return e, nil
}

func (r *ExportRuntime) ready(ctx context.Context, machine bool, out, diagnostics io.Writer) int {
	if file, ok := out.(*os.File); ok {
		pipe, release, err := pollablePipe(file)
		if err != nil {
			outputFailure(diagnostics)
			return 1
		}
		if release != nil {
			defer release()
		}
		defer closeOnCancel(ctx, pipe)()
		out = pipe
	}
	info := make([]EndpointInfo, 0, len(r.Endpoints))
	var text strings.Builder
	for _, item := range r.Endpoints {
		info = append(info, item.info)
		if !machine {
			scheme := "http"
			switch {
			case item.info.Protocol == "sftp" || item.info.Protocol == "ftp":
				scheme = item.info.Protocol
			case item.info.TLS:
				scheme = "https"
			}
			protocol := item.info.Protocol
			if protocol == "ftp" && item.info.TLS {
				protocol = "ftp (explicit TLS)"
			}
			fmt.Fprintf(&text, "Serving %s at %s://%s for %q (%s).\n", protocol, scheme, item.info.Listen, item.info.Root, item.info.Access)
		}
	}
	return r.app.respond(machine, out, diagnostics, struct {
		Status  string         `json:"status"`
		Exports []EndpointInfo `json:"exports"`
	}{"ready", info}, text.String(), nil)
}

func (r *ExportRuntime) Run(ctx context.Context) error {
	exits := make(chan string, len(r.Endpoints))
	for _, item := range r.Endpoints {
		go func() {
			_ = item.Server.Serve(item.Listener)
			exits <- item.info.Listen
		}()
	}
	select {
	case listen := <-exits:
		return &Problem{Code: "listener_failed", Message: fmt.Sprintf("Listener at %q exited unexpectedly.", listen)}
	case <-ctx.Done():
		return nil
	}
}

func (r *ExportRuntime) Shutdown(started bool, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var result error
	for _, item := range r.Endpoints {
		if item.Listener != nil {
			if err := item.Listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				result = errors.Join(result, r.app.serveProblem(err, "Listener at %q", item.info.Listen))
			}
		}
	}
	if started {
		// Leave part of the same deadline for forced cancellation and retirement.
		grace, finish := context.WithTimeout(ctx, timeout-timeout/5)
		drained := make(chan error, len(r.Endpoints))
		for _, item := range r.Endpoints {
			go func() {
				err := item.Server.Shutdown(grace)
				if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, net.ErrClosed) {
					drained <- nil
				} else {
					drained <- r.app.serveProblem(err, "Listener at %q", item.info.Listen)
				}
			}()
		}
		for range r.Endpoints {
			result = errors.Join(result, <-drained)
		}
		finish()
	}
	if r.cancel != nil {
		r.cancel()
	}
	if started {
		for _, item := range r.Endpoints {
			if err := item.Server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				result = errors.Join(result, r.app.serveProblem(err, "Listener at %q", item.info.Listen))
			}
		}
	}
	if r.Core != nil {
		closed := make(chan error, 1)
		go func() { closed <- r.Core.Close(ctx) }()
		for _, item := range r.Endpoints {
			if item.close != nil {
				if err := item.close(ctx); err != nil {
					result = errors.Join(result, r.app.serveProblem(err, "Adapter at %q", item.info.Listen))
				}
			}
		}
		if err := <-closed; err != nil {
			result = errors.Join(result, r.app.serveProblem(err, "Mount retirement"))
		}
	}
	return result
}

func (app *App) serveProblem(err error, where string, args ...any) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	p := *AppProblem(app, err)
	p.Message = fmt.Sprintf(where, args...) + ". " + p.Message
	return &p
}
