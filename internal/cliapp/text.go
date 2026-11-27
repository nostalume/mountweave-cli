package cliapp

import (
	"fmt"
	"strconv"
	"strings"

	core "github.com/nostalume/mountweave"
)

func displayText(name string) string {
	if strings.TrimSpace(name) != name || strings.IndexFunc(name, func(r rune) bool {
		return !strconv.IsPrint(r) || r == '"' || r == '\\'
	}) >= 0 {
		return strconv.Quote(name)
	}
	return name
}

func entryText(entry core.Entry) string {
	var lines strings.Builder
	fmt.Fprintf(&lines, "Path      %s\nType      %s\n", displayText(addressText(entry.Address)), entry.Kind)
	if entry.Size != nil {
		fmt.Fprintf(&lines, "Size      %d bytes\n", *entry.Size)
	}
	if entry.ModifiedAt != nil {
		fmt.Fprintf(&lines, "Modified  %s\n", entry.ModifiedAt.UTC().Format("2006-01-02 15:04:05 MST"))
	}
	return lines.String()
}
