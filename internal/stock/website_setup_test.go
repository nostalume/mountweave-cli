package stock

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
)

type websiteFixtureDriver struct {
	core.Driver
	rootID  string
	release func() error
}

func (d websiteFixtureDriver) RootID() string { return d.rootID }
func (d websiteFixtureDriver) Close() error   { return d.release() }

func TestWebsiteMountTransaction(t *testing.T) {
	if !cliapp.ManagedStoreSupported {
		t.Skip("managed storage is unavailable")
	}
	isolatedSettingsHome(t)
	config := filepath.Join(t.TempDir(), "mweave.json")
	var candidate, signInProfile string
	signIn := signInWebsite
	t.Cleanup(func() { signInWebsite = signIn })
	signInWebsite = func(_ context.Context, _, profile, entry string, ask cliapp.Prompt, _ io.Writer) error {
		if entry != "https://site.invalid" {
			t.Fatalf("unexpected login entry %q", entry)
		}
		candidate, signInProfile = profile, profile
		return cliapp.Confirm(ask, "Continue?")
	}

	ctx := t.Context()
	account, rootID := "identity", "folder-42"
	opens, closes := 0, 0
	var duringMount func()
	source := websiteSource{ID: "website-fixture", Name: "Fixture", Entry: "https://site.invalid",
		Open: func(_ context.Context, _ string, _ []chromedp.ExecAllocatorOption, profile string) (websiteSession, error) {
			if signInProfile != "" && profile != signInProfile {
				t.Fatalf("opened profile %q, want the login profile %q", profile, signInProfile)
			}
			signInProfile = ""
			opens++
			close := func() error { closes++; return nil }
			return websiteSession{
				Account: func(context.Context) (string, error) { return account, nil },
				Close:   close,
				Mount: func(_ context.Context, who, where, expected string, release func() error) (websiteDriver, error) {
					if duringMount != nil {
						duringMount()
					}
					if who != account || where != "/team" || expected != "" && expected != rootID {
						return nil, cliapp.AuthorizationScopeConflict()
					}
					return websiteFixtureDriver{rootID: rootID, release: release}, nil
				},
			}, nil
		},
	}
	app, err := cliapp.NewApp(nil, cliapp.SingleSource(source.ID, source.Name, source.route()))
	if err != nil {
		t.Fatal(err)
	}
	run := func(input string, ok bool, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		code := app.Run(ctx, append([]string{"--config", config}, args...), strings.NewReader(input), &out, &out, true)
		if (code == 0) != ok {
			t.Fatalf("%v => %d: %s", args, code, out.String())
		}
		return out.String()
	}
	save := func() cliapp.ConfigSnapshot {
		t.Helper()
		saved, err := cliapp.LoadConfig(config)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	assertGone := func(profile string) {
		t.Helper()
		if _, err := os.Stat(profile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed transaction retained candidate %q: %v", profile, err)
		}
		missing, err := cliapp.MissingGrant(filepath.Base(profile))
		if err != nil || !missing {
			t.Fatalf("failed transaction published candidate authorization: %v", err)
		}
	}

	run("website-fixture\ny\n/team\nn\n", false, "mount", "add", "/drive")
	assertGone(candidate)
	run("website-fixture\ny\n/team\ny\n", true, "mount", "add", "/drive")
	profile := candidate
	saved := save()
	ref := saved.Value.Mounts[0].Credential.Ref
	if ref != filepath.Base(profile) || opens != 2 || closes != 2 {
		t.Fatalf("unbalanced initial publication: %d opens, %d closes, reference %q", opens, closes, ref)
	}
	runtime, err := cliapp.OpenFor(app, ctx, saved, core.Path("/drive"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := cliapp.AcquireGrant(ref, false)
	if err == nil {
		_ = lease.Lock.Close()
		_ = cliapp.CloseRuntime(runtime)
		t.Fatal("the mounted session released its grant lock")
	}
	if err := cliapp.CloseRuntime(runtime); err != nil || opens != 3 || closes != 3 {
		t.Fatalf("runtime did not release the session: %d opens, %d closes: %v", opens, closes, err)
	}
	lease, err = cliapp.AcquireGrant(ref, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Lock.Close(); err != nil {
		t.Fatal(err)
	}

	if out := run("\n\n", true, "mount", "edit"); !strings.Contains(out, "Edit settings") ||
		candidate != profile || opens != 3 || closes != 3 || save().Digest != saved.Digest {
		t.Fatalf("editing ordinary settings reopened the website or changed authorization: %s", out)
	}

	rootID = "different-folder"
	run("2\ny\n", false, "mount", "edit", "/drive")
	assertGone(candidate)
	if _, err := os.Stat(profile); err != nil || save().Digest != saved.Digest || opens != closes {
		t.Fatalf("rejected root invalidated existing authorization: %v", err)
	}
	rootID = "folder-42"
	if err := os.Remove(profile); err != nil {
		t.Fatal(err)
	}
	action := cliapp.MountEditAction(saved.Path, "/drive")
	if out := run("", true, "mount", "list", "--json"); !strings.Contains(out, "interaction_required") ||
		!strings.Contains(out, "next_action") || opens != closes {
		t.Fatalf("listing did not diagnose the missing profile offline: %s", out)
	}
	if out := run("", false, "ls", "/drive"); !strings.Contains(out, action) || !strings.Contains(out, "interaction_required") {
		t.Fatalf("missing profile did not offer mount edit: %s", out)
	}
	if err := os.WriteFile(profile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if out := run("", false, "ls", "/drive"); strings.Contains(out, action) || !strings.Contains(out, "credential_unavailable") {
		t.Fatalf("invalid profile was reported as missing: %s", out)
	}
	if err := os.Remove(profile); err != nil {
		t.Fatal(err)
	}
	run("\ny\n", true, "mount", "edit", "/drive")
	if candidate == profile || save().Digest != saved.Digest || opens != closes {
		t.Fatal("recovery did not preserve the original mount and replace only its profile")
	}
	// A missing grant, or a Windows grant that this user cannot unprotect,
	// requires confirmation of the newly authenticated account.
	unreadable := []byte("fixture grant from a different user")
	if goruntime.GOOS == "windows" {
		err = os.WriteFile(lease.Path, unreadable, 0600)
	} else {
		err = os.Remove(lease.Path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if out := run("\ny\nn\n", false, "mount", "edit", "/drive"); !strings.Contains(out, "previous account could not be verified") ||
		save().Digest != saved.Digest {
		t.Fatalf("unverified old authorization did not require consent: %s", out)
	}
	assertGone(candidate)
	run("\ny\ny\n", true, "mount", "edit", "/drive")
	m := save().Value.Mounts[0]
	if m.Credential.Ref == ref || !bytes.Equal(m.Options, saved.Value.Mounts[0].Options) || opens != closes {
		t.Fatal("reconnect should replace only the selected credential reference")
	}
	if goruntime.GOOS == "windows" {
		if content, err := os.ReadFile(lease.Path); err != nil || !bytes.Equal(content, unreadable) {
			t.Fatalf("reconnect touched the unreadable grant: %v", err)
		}
	}

	configBytes, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := append(configBytes, '\n')
	duringMount = func() {
		if err := os.WriteFile(config, concurrent, 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("website-fixture\ny\n/team\ny\n", false, "mount", "add", "/other")
	assertGone(candidate)
	actual, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(actual, concurrent) || opens != closes {
		t.Fatalf("failed commit changed concurrent settings or retained the session: %v", err)
	}
}
