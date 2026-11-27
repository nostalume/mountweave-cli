package cliapp

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	core "github.com/nostalume/mountweave"
)

type Config struct {
	Version int           `json:"version"`
	Mounts  []MountRecord `json:"mounts"`
	Exports []Export      `json:"exports,omitempty"`
}

type MountRecord struct {
	At         string          `json:"at"`
	Driver     string          `json:"driver"`
	Connection string          `json:"connection,omitempty"`
	Options    json.RawMessage `json:"options"`
	Credential *CredentialRef  `json:"credential,omitempty"`
}

type CredentialRef struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

func (ref *CredentialRef) Valid() bool {
	return ref != nil && strings.TrimSpace(ref.Ref) != "" &&
		slices.Contains([]string{"env", "file", "profile", ManagedCredential}, ref.Kind)
}

func InvalidConfig(message string) error {
	return &Problem{Code: "invalid_config", Message: message}
}

func ConfigConflict() error {
	return &Problem{Code: "config_conflict", Message: "The saved configuration changed or the requested mount conflicts.", NextAction: "Review the saved mounts and retry."}
}

func validateRecord(m MountRecord) error {
	options := bytes.TrimSpace(m.Options)
	if core.ValidatePath(m.At) != nil || strings.TrimSpace(m.Driver) == "" || len(options) == 0 || options[0] != '{' {
		return InvalidConfig("A mount needs an absolute path, a Driver and an options object.")
	}
	if m.Credential != nil && !m.Credential.Valid() {
		return InvalidConfig("A mount credential must be a supported reference.")
	}
	if m.Driver == PoolSourceID {
		return validatePool(m)
	}
	return nil
}

func (c Config) validate() error {
	if c.Version != 1 || c.Mounts == nil {
		return InvalidConfig("Configuration requires version 1 and a mounts array.")
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
		for parent := name; parent != ""; parent = filepathToTreeParent(parent) {
			if seen[parent] {
				return InvalidConfig("Mount paths must be unique and non-nested.")
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
			return InvalidConfig("Export listeners must be unique.")
		}
		scope := e.scope()
		if core.ValidatePath(scope.root) != nil || !slices.ContainsFunc(paths, scope.containsMount) {
			if scope.keys {
				return InvalidConfig("An S3 export root must be the exact path of a saved object mount.")
			}
			return InvalidConfig("An export root must be within a saved mount.")
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
func DecodeObject(data []byte, value any) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' || !utf8.Valid(data) {
		return InvalidConfig("Expected a UTF-8 JSON object.")
	}
	scanner := json.NewDecoder(bytes.NewReader(data))
	scanner.UseNumber()
	if !scanJSON(scanner, reflect.TypeOf(value).Elem()) {
		return InvalidConfig("JSON contains invalid syntax, an unknown field or duplicate fields.")
	}
	if _, err := scanner.Token(); err != io.EOF {
		return InvalidConfig("Expected exactly one JSON object.")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return InvalidConfig("JSON has an unknown field or invalid field value.")
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

const (
	AliPanWebsiteSource = "alipan-web"
	MobilePanSource     = "mobilepan"
	UcSource            = "uc"
	QuarkSource         = "quark"
	LanZouSource        = "lanzou"
)

func savedSource(m MountRecord) (id, method string) {
	if m.Connection == "" {
		switch m.Driver {
		case AliPanWebsiteSource:
			return "alipan", "website"
		case "alipan", "baidupan", "guangyapan", "pan115", "pan123":
			return m.Driver, "open"
		}
	}
	return m.Driver, m.Connection
}

// Protect saved grants and legacy website records even with no matching source registered.
func (m MountRecord) hasProtectedState() bool {
	managed := m.Credential != nil && m.Credential.Kind == ManagedCredential
	return managed || m.AliPanWebsite() || m.Connection == "" &&
		slices.Contains([]string{MobilePanSource, UcSource, QuarkSource, LanZouSource}, m.Driver)
}

func (m MountRecord) AliPanWebsite() bool {
	id, method := savedSource(m)
	return id == "alipan" && method == "website"
}
