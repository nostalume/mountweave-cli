package cliapp

import (
	"net/url"
	"strings"

	core "github.com/nostalume/mountweave"
)

// mountedPath accepts CLI spelling. Core still validates the resulting path.
func mountedPath(value string, directory bool) string {
	value = strings.ReplaceAll(value, "\\", "/")
	if value != "" && value[0] != '/' {
		value = "/" + value
	}
	if directory && len(value) > 2 {
		value = strings.TrimSuffix(value, "/")
	}
	return value
}

func directoryTarget(operand string) bool {
	return !strings.HasPrefix(operand, "key:") && (strings.HasSuffix(operand, "/") || strings.HasSuffix(operand, "\\"))
}

func fileAddress(operand string, listing, directory bool) (core.Address, error) {
	raw, keyAddress := strings.CutPrefix(operand, "key:")
	if !keyAddress {
		return core.Path(mountedPath(operand, listing || directory)), nil
	}
	encodedMount, key, ok := strings.Cut(raw, ":")
	mount, err := url.PathUnescape(encodedMount)
	if !ok || err != nil {
		return core.Address{}, core.Fail(core.InvalidPath, nil)
	}
	if directory {
		return core.Address{}, core.Fail(core.UnsupportedOperation, nil)
	}
	mount = mountedPath(mount, true)
	if listing {
		return core.Prefix(mount, key), nil
	}
	return core.Key(mount, key), nil
}

func addressText(address core.Address) string {
	if address.Namespace() == core.KeyNamespace {
		space := strings.ReplaceAll(address.Space(), "%", "%25")
		space = strings.ReplaceAll(space, ":", "%3A")
		return "key:" + space + ":" + address.Value()
	}
	return address.Value()
}
