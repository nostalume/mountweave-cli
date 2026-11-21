//go:build windows || linux

package main

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/nostalume/mountweave-cli/internal/website"
)

func TestOpenProbeProfile(t *testing.T) {
	config := t.TempDir()
	t.Setenv("APPDATA", config)
	t.Setenv("XDG_CONFIG_HOME", config)
	parse := func(raw string) *url.URL {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	open := func(raw string, fresh bool) (string, bool, func() error) {
		t.Helper()
		profile, existed, release, err := openProbeProfile(parse(raw), fresh)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = release() })
		return profile, existed, release
	}

	primary, existed, releasePrimary := open("https://Drive.Fixture/folder", false)
	if existed {
		t.Fatal("first open incorrectly reported an existing profile")
	}
	if _, _, release, err := openProbeProfile(parse("https://drive.fixture:443/another/folder"), false); !errors.Is(err, errProbeProfileBusy) {
		if release != nil {
			_ = release()
		}
		t.Fatal("an equivalent origin acquired an active profile", err)
	}
	other, existed, _ := open("https://drive.fixture:444/folder", false)
	if existed || other == primary {
		t.Fatal("different HTTPS ports shared a profile")
	}
	if err := releasePrimary(); err != nil {
		t.Fatal(err)
	}
	again, existed, _ := open("https://drive.fixture:0443/list", false)
	if !existed || again != primary {
		t.Fatal("same normalized origin did not reopen its profile")
	}

	quarkDir := filepath.Join(config, "mweave-quark-check")
	legacy := filepath.Join(quarkDir, "website")
	for _, path := range []string{quarkDir, legacy} {
		if err := website.PrivateDirectory(path, true); err != nil {
			t.Fatal(err)
		}
	}
	quark, existed, _ := open("https://PAN.Quark.CN:443/list", false)
	alternateQuark, alternateExisted, _ := open("https://pan.quark.cn:444/list", false)
	if !existed || quark != legacy || alternateExisted || alternateQuark == legacy {
		t.Fatal("the existing Quark profile was not isolated to its original origin")
	}

	fresh, existed, remove := open("https://pan.quark.cn/", true)
	if existed || fresh == legacy {
		t.Fatal("an explicit fresh run reused its persistent profile")
	}
	if err := remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fresh browser profile remained after release")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("a fresh run removed the existing profile")
	}
}
