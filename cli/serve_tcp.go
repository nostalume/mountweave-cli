package cli

import (
	"context"
	"errors"
	"net"
	"sync"
)

type tcpExport struct {
	serveConn func(net.Conn)
	mu        sync.Mutex
	clients   map[net.Conn]struct{}
	stopping  bool
	done      chan struct{}
}

func newTCPExport(serve func(net.Conn)) *tcpExport {
	return &tcpExport{serveConn: serve, clients: make(map[net.Conn]struct{}), done: make(chan struct{})}
}

func (s *tcpExport) Serve(listener net.Listener) error {
	const maxConnections = 32
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.stopping || len(s.clients) >= maxConnections {
			s.mu.Unlock()
			_ = conn.Close()
			continue
		}
		s.clients[conn] = struct{}{}
		s.mu.Unlock()
		go func() {
			s.serveConn(conn)
			s.mu.Lock()
			delete(s.clients, conn)
			if s.stopping && len(s.clients) == 0 {
				close(s.done)
			}
			s.mu.Unlock()
		}()
	}
}

func (s *tcpExport) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.stopping {
		s.stopping = true
		if len(s.clients) == 0 {
			close(s.done)
		}
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *tcpExport) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	for conn := range s.clients {
		if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			err = errors.Join(err, closeErr)
		}
	}
	return err
}
