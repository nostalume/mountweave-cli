package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave/input"
	"github.com/nostalume/mountweave/protocols/s3"
	bolt "go.etcd.io/bbolt"
)

type s3MultipartOptions struct {
	StateDirectory   string `json:"state_directory"`
	MaxStagingBytes  int64  `json:"max_staging_bytes"`
	MaxObjectBytes   int64  `json:"max_object_bytes,omitempty"`
	RetentionSeconds int64  `json:"retention_seconds,omitempty"`
}

const (
	multipartBlockBytes = 8 << 20
	multipartRetention  = 24 * time.Hour
)

func (o s3MultipartOptions) limits() input.Options {
	const maxInputs = 4096
	block := min(int64(multipartBlockBytes), o.MaxStagingBytes/2)
	// Allow a block per small input, plus the blocks of large inputs.
	blocks := min(o.MaxStagingBytes/block, (1<<20)-maxInputs) + maxInputs
	return input.Options{MaxBytes: o.MaxStagingBytes, MaxInputs: maxInputs, MaxAppendBytes: block, MaxBlocks: int(blocks)}
}

func s3MultipartIdentity(saved configSnapshot, e export) (core.Principal, error) {
	for _, m := range saved.value.Mounts {
		if m.At != e.Root {
			continue
		}
		// Preserve JSON numbers and ignore presentation of the saved options.
		var options any
		decoder := json.NewDecoder(bytes.NewReader(m.Options))
		decoder.UseNumber()
		if err := decoder.Decode(&options); err != nil {
			return "", err
		}
		value, err := json.Marshal(struct {
			Bucket, AccessKey, ConfigDir, Root, Driver string
			Options                                    any
			Credential                                 *credentialRef
		}{e.S3.Bucket, e.Auth.AccessKeyID, filepath.Dir(saved.path), m.At, m.Driver, options, m.Credential})
		if err != nil {
			return "", err
		}
		return core.Principal(fmt.Sprintf("s3-multipart:%x", sha256.Sum256(value))), nil
	}
	return "", invalidConfig("S3 multipart requires an existing object mount.")
}

type s3MultipartState struct {
	db     *bolt.DB
	inputs *input.Store
}

func openS3Multipart(ctx context.Context, dir string, identity core.Principal, limits input.Options) (_ s3MultipartState, err error) {
	if err := ctx.Err(); err != nil {
		return s3MultipartState{}, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return s3MultipartState{}, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return s3MultipartState{}, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return s3MultipartState{}, invalidConfig("The S3 multipart state directory must be private to its owner.")
	}
	manifest := filepath.Join(dir, "manifest.db")
	if info, err := os.Lstat(manifest); err == nil {
		if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			return s3MultipartState{}, invalidConfig("The S3 multipart manifest must be a private regular file.")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return s3MultipartState{}, err
	}
	db, err := bolt.Open(manifest, 0600, &bolt.Options{Timeout: time.Second})
	if errors.Is(err, bolt.ErrTimeout) {
		return s3MultipartState{}, &problem{Code: "multipart_in_use", Message: "The S3 multipart state directory is already in use."}
	}
	if err != nil {
		return s3MultipartState{}, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, db.Close())
		}
	}()
	private := filepath.Join(dir, "inputs")
	if err := os.Mkdir(private, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return s3MultipartState{}, err
	}
	info, err = os.Lstat(private)
	if err != nil {
		return s3MultipartState{}, err
	}
	if !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return s3MultipartState{}, invalidConfig("S3 multipart inputs must use a private directory inside the state directory.")
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists([]byte("mweave.s3.binding.v1"))
		if err != nil {
			return err
		}
		key := []byte("identity")
		current := meta.Get(key)
		if current == nil {
			directory, err := os.Open(private)
			if err != nil {
				return err
			}
			entries, readErr := directory.Readdirnames(1)
			closeErr := directory.Close()
			if len(entries) != 0 {
				return invalidConfig("S3 multipart inputs exist without their binding manifest.")
			}
			if readErr != io.EOF || closeErr != nil {
				return errors.Join(readErr, closeErr)
			}
			return meta.Put(key, []byte(identity))
		}
		if string(current) != string(identity) {
			return &problem{Code: "multipart_binding_changed", Message: "S3 multipart state belongs to a different bucket, mount or access key ID.", NextAction: "Restore its original binding or use another private state directory."}
		}
		return nil
	}); err != nil {
		return s3MultipartState{}, err
	}
	inputs, err := input.Open(ctx, private, limits)
	if err != nil {
		return s3MultipartState{}, err
	}
	return s3MultipartState{db, inputs}, nil
}

func (s s3MultipartState) close() error {
	return errors.Join(s.inputs.Close(context.Background()), s.db.Close())
}

// The server can close sockets before their handlers finish. Keep the
// manifest locked until handlers and maintenance have stopped using it.
type s3MultipartExport struct {
	handler         *s3.Handler
	state           s3MultipartState
	cancel          context.CancelFunc
	maintenanceDone chan struct{}
	closed          chan struct{}
	closeErr        error

	mu      sync.Mutex
	active  int
	drained chan struct{}
}

func newS3MultipartExport(ctx context.Context, handler *s3.Handler, state s3MultipartState, logger *log.Logger) *s3MultipartExport {
	ctx, cancel := context.WithCancel(ctx)
	s := &s3MultipartExport{handler: handler, state: state, cancel: cancel, maintenanceDone: make(chan struct{}), closed: make(chan struct{})}
	go func() {
		defer close(s.maintenanceDone)
		timer := time.NewTicker(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.mu.Lock()
				idle := s.active == 0 && s.drained == nil
				s.mu.Unlock()
				if !idle {
					continue
				}
				pass, stop := context.WithTimeout(ctx, 5*time.Second)
				err := handler.RecoverMultipart(pass)
				stop()
				if err != nil && !errors.Is(err, context.Canceled) && err.Error() != "OperationAborted" {
					logger.Print("s3_multipart_cleanup: cleanup did not complete; it will be retried")
				}
			}
		}
	}()
	return s
}

func (s *s3MultipartExport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.drained != nil {
		s.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	s.active++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		if s.active == 0 && s.drained != nil {
			close(s.drained)
		}
		s.mu.Unlock()
	}()
	s.handler.ServeHTTP(w, r)
}

func (s *s3MultipartExport) close(ctx context.Context) error {
	s.mu.Lock()
	if s.drained == nil {
		s.drained = make(chan struct{})
		if s.active == 0 {
			close(s.drained)
		}
		s.cancel()
		go func() {
			<-s.drained
			<-s.maintenanceDone
			s.closeErr = s.state.close()
			close(s.closed)
		}()
	}
	s.mu.Unlock()
	select {
	case <-s.closed:
		return s.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
