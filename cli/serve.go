package cli

import (
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
	"strings"
	"syscall"
	"time"

	core "github.com/nostalume/mountweave"
)

const shutdownTimeout = 5 * time.Second

type endpointInfo struct {
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

type tlsExport struct{ *http.Server }

func (s tlsExport) Serve(l net.Listener) error { return s.ServeTLS(l, "", "") }

type endpoint struct {
	info     endpointInfo
	server   exportServer
	listener net.Listener
	close    func(context.Context) error
}

type exportRuntime struct {
	core      *core.Core
	endpoints []*endpoint
	cancel    context.CancelFunc
}

func (app *App) runServe(ctx context.Context, saved configSnapshot, machine bool, out, diagnostics io.Writer) int {
	runtime, err := app.prepareExports(ctx, saved, diagnostics)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		cleanupErr := runtime.shutdown(false, shutdownTimeout)
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return reportServeStop(diagnostics, nil, cleanupErr)
		}
		code := respond(machine, out, diagnostics, nil, "", err)
		_ = reportServeStop(diagnostics, nil, cleanupErr)
		return code
	}

	if code := runtime.ready(ctx, machine, out, diagnostics); code != 0 {
		_ = reportServeStop(diagnostics, nil, runtime.shutdown(false, shutdownTimeout))
		return code
	}
	err = runtime.run(ctx)
	return reportServeStop(diagnostics, err, runtime.shutdown(true, shutdownTimeout))
}

func (app *App) prepareExports(ctx context.Context, saved configSnapshot, diagnostics io.Writer) (runtime exportRuntime, err error) {
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	logger := log.New(diagnostics, "", 0)
	if len(saved.value.Exports) == 0 {
		return runtime, invalidConfig("No protocol exports are configured. Use mweave export add.")
	}
	if err := checkStorageLocations(saved, true); err != nil {
		return runtime, err
	}
	scopes := make(map[core.Principal]exportScope, len(saved.value.Exports))
	runtime.core, err = core.New(core.Options{MaxCursors: 128, Authorize: func(_ context.Context, p core.Principal, a core.Action, addr core.Address) error {
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
	principals := make(map[string]core.Principal, len(saved.value.Exports))
	for _, e := range saved.value.Exports {
		if err := e.checkAvailability(); err != nil {
			return runtime, serveProblem(err, "Export at %q", e.Listen)
		}
		if err := e.checkCredential(saved); err != nil {
			return runtime, serveProblem(err, "Export at %q", e.Listen)
		}
		p := core.Principal(e.Listen)
		if e.S3 != nil && e.S3.Multipart != nil {
			p, err = s3MultipartIdentity(saved, e)
			if err != nil {
				return runtime, serveProblem(err, "Export at %q", e.Listen)
			}
		}
		principals[e.Listen], scopes[p] = p, e.scope()
	}

	ctx = withAliPanWebsiteSessions(ctx)
	for _, m := range saved.value.Mounts {
		for _, e := range saved.value.Exports {
			if e.containsMount(m.At) {
				if err := app.install(ctx, runtime.core, saved, m); err != nil {
					return runtime, serveProblem(err, "Mount %q", m.At)
				}
				break
			}
		}
	}
	for _, e := range saved.value.Exports {
		if e.Protocol == "s3" {
			// Resolve the native key binding without opening a cursor or reading storage.
			if _, err := runtime.core.Capture(ctx, core.AddressAccess{Address: core.Prefix(e.Root, "")}); err != nil {
				if core.CodeOf(err) == core.UnsupportedOperation {
					err = invalidConfig("S3 requires a mount with native object keys.")
				}
				return runtime, serveProblem(err, "Export root %q at %q", e.Root, e.Listen)
			}
			continue
		}
		entry, err := runtime.core.Stat(ctx, principals[e.Listen], core.Path(e.Root))
		if err == nil && (entry.Kind != core.Directory || entry.Virtual) {
			err = core.Fail(core.NotDirectory, nil)
		}
		if err != nil {
			return runtime, serveProblem(err, "Export root %q at %q", e.Root, e.Listen)
		}
	}

	for _, e := range saved.value.Exports {
		if err := ctx.Err(); err != nil {
			return runtime, err
		}
		item, err := prepareEndpoint(ctx, requests, saved, runtime.core, e, principals[e.Listen], logger)
		if err != nil {
			return runtime, serveProblem(err, "Export at %q", e.Listen)
		}
		runtime.endpoints = append(runtime.endpoints, item)
	}
	for _, item := range runtime.endpoints {
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
			var configErr *problem
			if !errors.As(err, &configErr) {
				err = &problem{Code: "listener_failed", Message: "The address could not be bound.", NextAction: "Check whether the address is available."}
			}
			return runtime, serveProblem(err, "Listener at %q", item.info.Listen)
		}
		item.listener = l
		item.info.Listen = l.Addr().String()
	}
	return runtime, nil
}

// ListenConfig supplies the resolved address before the socket is bound.
func requireLoopback(_ string, address string, _ syscall.RawConn) error {
	host, _, _ := net.SplitHostPort(address)
	host, _, _ = strings.Cut(host, "%")
	if !net.ParseIP(host).IsLoopback() {
		return invalidConfig("Cleartext must bind to a loopback address.")
	}
	return nil
}

func prepareEndpoint(startup, ctx context.Context, saved configSnapshot, c *core.Core, e export, p core.Principal, logger *log.Logger) (*endpoint, error) {
	base := filepath.Dir(saved.path)
	access := e.Access
	if access == "" {
		access = "read"
	}
	item := &endpoint{info: endpointInfo{e.Protocol, e.Listen, e.Root, access, e.TLS != nil}}
	if e.Protocol == "sftp" {
		server, err := prepareSFTPExport(ctx, c, e, p, base)
		if err != nil {
			return nil, err
		}
		item.server, item.close = server, server.Shutdown
		return item, nil
	}
	secret, err := loadCredential(e.Auth.Credential, base)
	if err != nil {
		return nil, err
	}
	var tlsConfig *tls.Config
	if e.TLS != nil {
		cert, err := tls.LoadX509KeyPair(hostPath(base, e.TLS.CertFile), hostPath(base, e.TLS.KeyFile))
		if err != nil {
			return nil, &problem{Code: "invalid_config", Message: "The TLS certificate and key could not be loaded.", NextAction: "Check the certificate and key files."}
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}
	if e.Protocol == "ftp" {
		server, err := prepareFTPExport(ctx, c, e, p, secret, tlsConfig)
		if err != nil {
			return nil, err
		}
		item.server, item.close = server, server.Shutdown
		return item, nil
	}
	server := &http.Server{
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		// net/http can log peer input and panic values. Only host-owned,
		// sanitized errors are emitted by this process.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	item.server = server
	if tlsConfig != nil {
		item.server = tlsExport{server}
	}
	var handler http.Handler
	var closeHandler func(context.Context) error
	if e.Protocol == "s3" {
		// ServeMux path cleaning would change the signed request target.
		handler, closeHandler, err = s3ExportHandler(startup, ctx, saved, c, e, p, secret, logger)
	} else {
		handler, closeHandler, err = exportHandler(c, e, p, secret)
	}
	if err != nil {
		return nil, err
	}
	server.Handler, item.close = handler, closeHandler
	return item, nil
}

func (r *exportRuntime) ready(ctx context.Context, machine bool, out, diagnostics io.Writer) int {
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
	info := make([]endpointInfo, 0, len(r.endpoints))
	var text strings.Builder
	for _, item := range r.endpoints {
		info = append(info, item.info)
		if !machine {
			scheme := "http"
			switch item.info.Protocol {
			case "sftp", "ftp":
				scheme = item.info.Protocol
			default:
				if item.info.TLS {
					scheme = "https"
				}
			}
			protocol := item.info.Protocol
			if protocol == "ftp" && item.info.TLS {
				protocol = "ftp (explicit TLS)"
			}
			fmt.Fprintf(&text, "Serving %s at %s://%s for %q (%s).\n", protocol, scheme, item.info.Listen, item.info.Root, item.info.Access)
		}
	}
	return respond(machine, out, diagnostics, struct {
		Status  string         `json:"status"`
		Exports []endpointInfo `json:"exports"`
	}{"ready", info}, text.String(), nil)
}

func (r *exportRuntime) run(ctx context.Context) error {
	exits := make(chan error, len(r.endpoints))
	for _, item := range r.endpoints {
		go func() {
			_ = item.server.Serve(item.listener)
			exits <- &problem{Code: "listener_failed", Message: fmt.Sprintf("Listener at %q exited unexpectedly.", item.info.Listen)}
		}()
	}
	select {
	case err := <-exits:
		return err
	case <-ctx.Done():
		return nil
	}
}

func (r *exportRuntime) shutdown(started bool, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var result error
	for _, item := range r.endpoints {
		if item.listener != nil {
			if err := item.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				result = errors.Join(result, serveProblem(err, "Listener at %q", item.info.Listen))
			}
		}
	}
	if started {
		// Leave part of the same deadline for forced cancellation and retirement.
		grace, finish := context.WithTimeout(ctx, timeout-timeout/5)
		drained := make(chan error, len(r.endpoints))
		for _, item := range r.endpoints {
			go func() {
				err := item.server.Shutdown(grace)
				if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, net.ErrClosed) {
					drained <- nil
				} else {
					drained <- serveProblem(err, "Listener at %q", item.info.Listen)
				}
			}()
		}
		for range r.endpoints {
			result = errors.Join(result, <-drained)
		}
		finish()
	}
	if r.cancel != nil {
		r.cancel()
	}
	if started {
		for _, item := range r.endpoints {
			if err := item.server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				result = errors.Join(result, serveProblem(err, "Listener at %q", item.info.Listen))
			}
		}
	}
	if r.core != nil {
		closed := make(chan error, 1)
		go func() { closed <- r.core.Close(ctx) }()
		for _, item := range r.endpoints {
			if item.close != nil {
				if err := item.close(ctx); err != nil {
					result = errors.Join(result, serveProblem(err, "Adapter at %q", item.info.Listen))
				}
			}
		}
		if err := <-closed; err != nil {
			result = errors.Join(result, serveProblem(err, "Mount retirement"))
		}
	}
	return result
}

func serveProblem(err error, where string, args ...any) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	p := *publicError(err)
	p.Message = fmt.Sprintf(where, args...) + ". " + p.Message
	return &p
}

func reportServeStop(diagnostics io.Writer, failure, cleanup error) int {
	if failure != nil {
		printProblem(diagnostics, publicError(failure))
	}
	if cleanup != nil {
		fmt.Fprintf(diagnostics, "shutdown_failed: %v\n", cleanup)
	}
	if failure != nil || cleanup != nil {
		return 1
	}
	return 0
}
