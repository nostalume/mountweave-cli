package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	core "github.com/nostalume/mountweave"
)

type config struct {
	Version int           `json:"version"`
	Mounts  []mountRecord `json:"mounts"`
	Exports []export      `json:"exports,omitempty"`
}

type mountRecord struct {
	At         string          `json:"at"`
	Driver     string          `json:"driver"`
	Connection string          `json:"connection,omitempty"`
	Options    json.RawMessage `json:"options"`
	Credential *credentialRef  `json:"credential,omitempty"`
}

type credentialRef struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

func (ref *credentialRef) valid() bool {
	return ref != nil && strings.TrimSpace(ref.Ref) != "" &&
		slices.Contains([]string{"env", "file", "profile", managedCredential}, ref.Kind)
}

func invalidConfig(message string) error {
	return &problem{Code: "invalid_config", Message: message}
}

func configConflict() error {
	return &problem{Code: "config_conflict", Message: "The saved configuration changed or the requested mount conflicts.", NextAction: "Review the saved mounts and retry."}
}

func validateRecord(m mountRecord) error {
	if core.ValidatePath(m.At) != nil || strings.TrimSpace(m.Driver) == "" ||
		len(bytes.TrimSpace(m.Options)) == 0 || bytes.TrimSpace(m.Options)[0] != '{' {
		return invalidConfig("A mount needs an absolute path, a Driver and an options object.")
	}
	if m.Credential != nil && !m.Credential.valid() {
		return invalidConfig("A mount credential must be a supported reference.")
	}
	if m.Driver == poolSourceID {
		if m.Connection != "" {
			return invalidConfig("Pool connection methods belong to its member records.")
		}
		_, err := decodePool(m)
		return err
	}
	return nil
}

func (c config) validate() error {
	if c.Version != 1 || c.Mounts == nil {
		return invalidConfig("Configuration requires version 1 and a mounts array.")
	}
	paths := make([]string, 0, len(c.Mounts))
	for _, m := range c.Mounts {
		if err := validateRecord(m); err != nil {
			return err
		}
		paths = append(paths, m.At)
	}
	slices.Sort(paths)
	seen := make(map[string]bool, len(paths))
	for _, name := range paths {
		if seen[name] {
			return invalidConfig("Mount paths must be unique and non-nested.")
		}
		for parent := filepathToTreeParent(name); parent != ""; parent = filepathToTreeParent(parent) {
			if seen[parent] {
				return invalidConfig("Mount paths must be unique and non-nested.")
			}
		}
		seen[name] = true
	}
	listeners := make(map[string]bool, len(c.Exports))
	for _, e := range c.Exports {
		if err := e.validate(); err != nil {
			return err
		}
		if listeners[e.Listen] {
			return invalidConfig("Export listeners must be unique.")
		}
		if core.ValidatePath(e.Root) != nil || !slices.ContainsFunc(paths, func(m string) bool {
			return e.containsMount(m)
		}) {
			if e.Protocol == "s3" {
				return invalidConfig("An S3 export root must be the exact path of a saved object mount.")
			}
			return invalidConfig("An export root must be within a saved mount.")
		}
		listeners[e.Listen] = true
	}
	return nil
}

func filepathToTreeParent(name string) string {
	if name == "/" {
		return ""
	}
	i := strings.LastIndexByte(name, '/')
	if i == 0 {
		return "/"
	}
	return name[:i]
}

// Decode validates object spelling and duplicates before interpreting values.
func decodeObject(data []byte, value any) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' || !utf8.Valid(data) {
		return invalidConfig("Expected a UTF-8 JSON object.")
	}
	scanner := json.NewDecoder(bytes.NewReader(data))
	scanner.UseNumber()
	if !scanJSON(scanner, reflect.TypeOf(value).Elem()) {
		return invalidConfig("JSON contains invalid syntax, an unknown field or duplicate fields.")
	}
	if _, err := scanner.Token(); err != io.EOF {
		return invalidConfig("Expected exactly one JSON object.")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return invalidConfig("JSON has an unknown field or invalid field value.")
	}
	return nil
}

func scanJSON(d *json.Decoder, shape reflect.Type) bool {
	if shape != nil && shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	token, err := d.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		keys := map[string]bool{}
		for d.More() {
			raw, err := d.Token()
			key, ok := raw.(string)
			field, known := jsonField(shape, key)
			if err != nil || !ok || !known || keys[key] || !scanJSON(d, field) {
				return false
			}
			keys[key] = true
		}
	case json.Delim('['):
		var element reflect.Type
		if shape != nil && (shape.Kind() == reflect.Slice || shape.Kind() == reflect.Array) {
			element = shape.Elem()
		}
		for d.More() {
			if !scanJSON(d, element) {
				return false
			}
		}
	default:
		return true
	}
	_, err = d.Token()
	return err == nil
}

// Only the declared spelling is accepted for typed configuration. Raw options
// are checked against the selected Driver's type when they are used.
func jsonField(shape reflect.Type, name string) (reflect.Type, bool) {
	if shape == nil || shape.Kind() != reflect.Struct {
		return nil, true
	}
	for i := 0; i < shape.NumField(); i++ {
		field := shape.Field(i)
		key, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if key == name && key != "" && key != "-" {
			return field.Type, true
		}
	}
	return nil, false
}

type configSnapshot struct {
	path   string
	value  config
	digest [32]byte
	exists bool
}

func loadConfig(name string) (configSnapshot, error) {
	name, err := resolveConfigPath(name)
	if err != nil {
		return configSnapshot{}, invalidConfig("The configuration path could not be resolved.")
	}
	data, exists, err := readConfig(name)
	s := configSnapshot{path: name, exists: exists, digest: sha256.Sum256(data)}
	if err != nil {
		return s, invalidConfig("The configuration file could not be read.")
	}
	if !exists {
		s.value = config{Version: 1, Mounts: []mountRecord{}}
		return s, nil
	}
	if err := decodeObject(data, &s.value); err != nil {
		return s, err
	}
	return s, s.value.validate()
}

func checkStorageLocations(saved configSnapshot, checkPool bool) error {
	mounts, exports := saved.value.Mounts, saved.value.Exports
	if slices.ContainsFunc(mounts, func(m mountRecord) bool {
		return slices.ContainsFunc(sourceRecords(m), func(source mountRecord) bool {
			return source.aliPanWebsite() || source.Driver == lanZouSource || source.Driver == mobilePanSource
		})
	}) {
		if err := checkWebsiteStorage(saved); err != nil {
			return err
		}
	}
	checkPool = checkPool && slices.ContainsFunc(mounts, func(m mountRecord) bool { return m.Driver == poolSourceID })
	multipart := slices.ContainsFunc(exports, func(e export) bool { return e.S3 != nil && e.S3.Multipart != nil })
	if !checkPool && !multipart {
		return nil
	}
	base := filepath.Dir(saved.path)
	const poolPathError = "A Pool or Local path could not be resolved."
	resolve := func(path, message string) (string, error) {
		resolved, err := resolveConfigPath(path)
		if err != nil {
			return "", invalidConfig(message)
		}
		return resolved, nil
	}
	type location struct {
		path    string
		private bool
		local   bool
		offline bool
	}
	locations := []location{{path: saved.path}}
	addLocal := func(options json.RawMessage, private, offline bool) error {
		var local localOptions
		if err := decodeObject(options, &local); err != nil {
			return err
		}
		locations = append(locations, location{path: hostPath(base, local.Root), private: private, local: true, offline: offline})
		return nil
	}
	for _, m := range mounts {
		switch m.Driver {
		case "local":
			if err := addLocal(m.Options, false, false); err != nil {
				return err
			}
		case poolSourceID:
			var opts poolOptions
			if err := json.Unmarshal(m.Options, &opts); err != nil {
				return err
			}
			if checkPool {
				locations = append(locations, location{path: hostPath(base, opts.MetadataPath), private: true})
			}
			for _, member := range opts.Members {
				if member.Driver == "local" {
					if err := addLocal(member.Options, true, member.Offline); err != nil {
						return err
					}
				}
			}
		}
	}
	if checkPool {
		for i := 1; i < len(locations); i++ {
			if locations[i].offline {
				continue
			}
			path, err := resolve(locations[i].path, poolPathError)
			if err != nil {
				return err
			}
			locations[i].path = path
		}
		for i, a := range locations {
			for _, b := range locations[i+1:] {
				if (a.private || b.private) && hostPathsOverlap(a.path, b.path) {
					return invalidConfig("Pool metadata and private Local roots must be separate from other mounts and configuration.")
				}
			}
		}
	}
	if !multipart {
		return nil
	}
	var states []string
	for _, e := range exports {
		if e.S3 == nil || e.S3.Multipart == nil {
			continue
		}
		state, err := resolve(hostPath(base, e.S3.Multipart.StateDirectory), "The S3 multipart state directory could not be resolved.")
		if err != nil {
			return err
		}
		for _, other := range states {
			if hostPathsOverlap(state, other) {
				return invalidConfig("S3 multipart exports need separate state directories.")
			}
		}
		if withinHostPath(saved.path, state) {
			return invalidConfig("S3 multipart state must be separate from the configuration.")
		}
		states = append(states, state)
	}
	for _, loc := range locations {
		if !loc.local {
			continue
		}
		root := loc.path
		// Pool checks offline members at their configured paths. Multipart
		// state must also be isolated from their resolved Local roots.
		if !checkPool || loc.offline {
			var err error
			root, err = resolve(root, "A Local mount path could not be resolved.")
			if err != nil {
				return err
			}
		}
		for _, state := range states {
			if hostPathsOverlap(state, root) {
				return invalidConfig("S3 multipart state must be outside Local mount roots.")
			}
		}
	}
	return nil
}

// checkWebsiteStorage keeps authenticated browser state outside Local and Pool sources.
func checkWebsiteStorage(saved configSnapshot) error {
	user, err := os.UserConfigDir()
	if err != nil {
		return grantUnavailable()
	}
	state, err := resolveConfigPath(filepath.Join(user, "mweave", "credentials", "website"))
	if err != nil {
		return grantUnavailable()
	}
	base := filepath.Dir(saved.path)
	for _, mount := range saved.value.Mounts {
		for _, source := range sourceRecords(mount) {
			if source.Driver != "local" {
				continue
			}
			var local localOptions
			if err := decodeObject(source.Options, &local); err != nil {
				return err
			}
			root, err := resolveConfigPath(hostPath(base, local.Root))
			if err != nil {
				return invalidConfig("The Local root could not be resolved.")
			}
			if hostPathsOverlap(state, root) {
				return invalidConfig("A Local mount overlaps protected website session data.")
			}
		}
	}
	return nil
}

func hostPathsOverlap(a, b string) bool {
	return withinHostPath(a, b) || withinHostPath(b, a)
}

func withinHostPath(name, root string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolveConfigPath(name string) (string, error) {
	name, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	var missing []string
	for {
		_, err := os.Lstat(name)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(name)
			slices.Reverse(missing)
			return filepath.Join(append([]string{resolved}, missing...)...), err
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(name)
		if parent == name {
			return "", err
		}
		missing = append(missing, filepath.Base(name))
		name = parent
	}
}

func readConfig(name string) ([]byte, bool, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("configuration must be a regular file")
	}
	f, err := openReplaceableFile(name)
	if err != nil {
		return nil, true, err
	}
	data, err := io.ReadAll(f)
	return data, true, errors.Join(err, f.Close())
}

func (s configSnapshot) save(ctx context.Context) (applied bool, err error) {
	if err := s.value.validate(); err != nil {
		return false, err
	}
	data, err := json.MarshalIndent(s.value, "", "  ")
	if err != nil {
		return false, err
	}
	lock, err := s.lockUnchanged(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	stage, err := stageFile(filepath.Dir(s.path), ".mweave-config-*", append(data, '\n'))
	if err != nil {
		return false, err
	}
	defer func() {
		if !applied {
			err = errors.Join(err, os.Remove(stage))
		}
	}()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := replaceConfig(stage, s.path, s.exists); err != nil {
		return false, err
	}
	return true, nil
}

func (s configSnapshot) lockUnchanged(ctx context.Context) (_ *os.File, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Keep the lock file's identity stable while other processes may wait on it.
	lock, err := os.OpenFile(filepath.Join(dir, "."+filepath.Base(s.path)+".mweave.lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, lock.Close())
		}
	}()
	if _, err := waitFileLock(ctx, lock); err != nil {
		return nil, err
	}
	current, exists, err := readConfig(s.path)
	if err != nil {
		return nil, err
	}
	if exists != s.exists || sha256.Sum256(current) != s.digest {
		return nil, configConflict()
	}
	return lock, nil
}

// With no context, attempt the lock once.
func waitFileLock(ctx context.Context, file *os.File) (bool, error) {
	for {
		if ctx != nil && ctx.Err() != nil {
			return false, ctx.Err()
		}
		ok, err := tryConfigLock(file)
		if ok || err != nil || ctx == nil {
			return ok, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// stageFile syncs a private file before a caller publishes it in the same directory.
func stageFile(dir, pattern string, data []byte) (string, error) {
	temp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	if _, err := temp.Write(data); err != nil {
		return "", errors.Join(err, temp.Close(), os.Remove(temp.Name()))
	}
	if err := errors.Join(temp.Sync(), temp.Close()); err != nil {
		return "", errors.Join(err, os.Remove(temp.Name()))
	}
	return temp.Name(), nil
}
