package cliapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

type inspectedGrant struct {
	data []byte
	err  error

	route   *Route
	options json.RawMessage
	problem error
}

// Only records referenced by this command are inspected. The cache releases
// their contents before listing or asking the user for input.
type mountInspection map[string]inspectedGrant

func (inspected mountInspection) clear() {
	for _, grant := range inspected {
		clear(grant.data)
	}
}

func (inspected mountInspection) credential(m MountRecord, route *Route, base string) error {
	ref := m.Credential
	if ref == nil {
		if route.InspectWebsite != nil {
			return missingManagedGrant
		}
		return nil
	}
	switch ref.Kind {
	case "env", "file":
		return inspectExternalCredential(*ref, base)
	case ManagedCredential:
		grant, ok := inspected[ref.Ref]
		if !ok {
			grant.data, grant.err = InspectGrant(ref.Ref)
			inspected[ref.Ref] = grant
		}
		if grant.err != nil {
			return grant.err
		}
		if grant.route == route && (route.InspectWebsite == nil || bytes.Equal(grant.options, m.Options)) {
			return grant.problem
		}
		grant.route, grant.options = route, m.Options
		grant.problem = checkManagedCredential(m, route, grant.data)
		inspected[ref.Ref] = grant
		return grant.problem
	}
	return nil
}

func checkManagedCredential(m MountRecord, route *Route, data []byte) error {
	id := m.Credential.Ref
	if route.InspectWebsite != nil {
		return route.InspectWebsite(id, m.Options, func() ([]byte, error) { return bytes.Clone(data), nil })
	}
	if route.managed.available() {
		var record struct {
			ID        string        `json:"id"`
			Provider  string        `json:"provider"`
			SecretRef CredentialRef `json:"secret_ref"`
		}
		if json.Unmarshal(data, &record) != nil || record.ID != id ||
			record.Provider != route.managed.Provider && (record.Provider != "" || route.managed.Read == nil) {
			return GrantUnavailable()
		}
		if route.managed.Kind == ApplicationGrant {
			if !record.SecretRef.Valid() || record.SecretRef.Kind != "env" && record.SecretRef.Kind != "file" {
				return GrantUnavailable()
			}
			return inspectExternalCredential(record.SecretRef, "")
		}
	}
	return nil
}

func inspectExternalCredential(ref CredentialRef, base string) error {
	if ref.Kind == "env" {
		if _, err := LoadCredential(&ref, base); err != nil {
			return &Problem{Code: "credential_unavailable",
				Message:    fmt.Sprintf("Environment variable %q needs a credential.", ref.Ref),
				NextAction: AppProblem(nil, err).NextAction}
		}
		return nil
	}
	name := HostPath(base, ref.Ref)
	info, err := os.Stat(name)
	if err == nil && info.Mode().IsRegular() {
		return nil
	}
	message := "Credential file is unavailable."
	if errors.Is(err, os.ErrNotExist) {
		message = "Credential file is missing."
	}
	return &Problem{Code: "credential_unavailable", Message: message,
		NextAction: fmt.Sprintf("Restore credential file %q or edit the mount to select another reference.", name)}
}

func inspectMount(m MountRecord, route *Route, base string) error {
	inspected := make(mountInspection)
	defer inspected.clear()
	return inspected.credential(m, route, base)
}

func (app *App) mountSummaries(saved ConfigSnapshot) []mountSummary {
	items := make([]mountSummary, 0, len(saved.Value.Mounts))
	inspected := make(mountInspection)
	defer inspected.clear()
	base := filepath.Dir(saved.Path)
	for _, m := range saved.Value.Mounts {
		route, err := app.mountRoute(m)
		item := mountSummary{At: m.At, Driver: m.Driver, DriverAvailable: err == nil, Connection: m.Connection}
		if err == nil {
			if err = inspected.credential(m, route, base); err != nil {
				problem := *AppProblem(app, route.ProjectError(err))
				problem.addMountRecovery(saved.Path, m)
				item.Problem = &problem
			}
		}
		items = append(items, item)
	}
	return items
}

func (m mountSummary) label() string {
	source, method := savedSource(MountRecord{Driver: m.Driver, Connection: m.Connection})
	label := displayText(m.At) + "  ·  " + displayText(source)
	if method != "" {
		label += " (" + displayText(method) + ")"
	}
	if !m.DriverAvailable {
		label += "  ·  unavailable in this build"
	}
	if m.Problem != nil {
		label += "  ·  " + m.Problem.Message
	}
	return label
}

func (app *App) chooseMount(saved ConfigSnapshot, ask Prompt) (string, error) {
	items := slices.DeleteFunc(app.mountSummaries(saved), func(m mountSummary) bool { return !m.DriverAvailable })
	if len(items) == 0 {
		return "", usage("No saved mounts are editable in this build.")
	}
	if len(items) == 1 {
		return items[0].At, nil
	}
	slices.SortStableFunc(items, func(a, b mountSummary) int {
		switch {
		case a.Problem != nil && b.Problem == nil:
			return -1
		case a.Problem == nil && b.Problem != nil:
			return 1
		default:
			return 0
		}
	})
	choices, labels := make([]string, len(items)), make([]string, len(items))
	for i, item := range items {
		choices[i], labels[i] = strconv.Itoa(i+1), item.label()
	}
	chosen, err := askMenu(ask, "Mount to edit", "", choices, labels)
	if err != nil {
		return "", err
	}
	return items[slices.Index(choices, chosen)].At, nil
}
