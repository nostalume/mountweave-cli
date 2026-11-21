package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nostalume/mountweave-cli/internal/filelock"
	"github.com/nostalume/mountweave-cli/internal/website"
)

var errProbeProfileBusy = errors.New("Another browser check is using the isolated profile for this website.")

// openProbeProfile owns a private browser profile for entry's HTTPS origin.
// Fresh profiles are temporary. Release the profile after the browser exits.
func openProbeProfile(entry *url.URL, fresh bool) (profile string, existed bool, release func() error, err error) {
	origin := probeOrigin(entry)
	if origin == "" {
		return "", false, nil, errors.New("Use an HTTPS website to open an isolated browser profile.")
	}
	if fresh {
		return openTemporaryProbeProfile()
	}

	config, err := os.UserConfigDir()
	if err != nil || !filepath.IsAbs(config) || os.MkdirAll(config, 0700) != nil {
		return "", false, nil, website.ErrUnsafeDirectory
	}
	// Preserve the already authorized, dedicated Quark probe's directory.
	dir := filepath.Join(config, "mweave-quark-check")
	if origin != "https://pan.quark.cn:443" {
		root := filepath.Join(config, "mweave-browser-check")
		if website.PrivateDirectory(root, true) != nil {
			return "", false, nil, website.ErrUnsafeDirectory
		}
		digest := sha256.Sum256([]byte(origin))
		dir = filepath.Join(root, hex.EncodeToString(digest[:16]))
	}
	if website.PrivateDirectory(dir, true) != nil {
		return "", false, nil, website.ErrUnsafeDirectory
	}

	// Keep the lock file stable while another process attempts to acquire it.
	lock, err := os.OpenFile(filepath.Join(dir, ".probe.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", false, nil, website.ErrUnsafeDirectory
	}
	unlock := sync.OnceValue(func() error {
		if lock.Close() != nil {
			return errors.New("Could not release the isolated browser profile.")
		}
		return nil
	})
	held, err := filelock.Try(lock)
	if err != nil {
		return "", false, nil, errors.Join(website.ErrUnsafeDirectory, unlock())
	}
	if !held {
		return "", false, nil, errors.Join(errProbeProfileBusy, unlock())
	}
	profile = filepath.Join(dir, "website")
	_, statErr := os.Lstat(profile)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) || website.PrivateDirectory(profile, true) != nil {
		return "", false, nil, errors.Join(website.ErrUnsafeDirectory, unlock())
	}
	return profile, statErr == nil, unlock, nil
}

func openTemporaryProbeProfile() (string, bool, func() error, error) {
	dir, err := os.MkdirTemp("", "mweave-browser-check-")
	if err != nil {
		return "", false, nil, website.ErrUnsafeDirectory
	}
	remove := func() error {
		if os.RemoveAll(dir) != nil {
			return errors.New("Could not remove the temporary browser profile.")
		}
		return nil
	}
	profile := filepath.Join(dir, "website")
	if website.PrivateDirectory(profile, true) != nil {
		return "", false, nil, errors.Join(website.ErrUnsafeDirectory, remove())
	}
	return profile, false, remove, nil
}

func probeOrigin(entry *url.URL) string {
	if entry == nil || entry.Scheme != "https" || entry.User != nil || entry.Opaque != "" {
		return ""
	}
	host := strings.ToLower(entry.Hostname())
	if host == "" {
		return ""
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	port := "443"
	if raw := entry.Port(); raw != "" {
		number, err := strconv.ParseUint(raw, 10, 16)
		if err != nil {
			return ""
		}
		port = strconv.FormatUint(number, 10)
	}
	return "https://" + net.JoinHostPort(host, port)
}
