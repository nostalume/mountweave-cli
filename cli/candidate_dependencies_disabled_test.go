//go:build !mweave_candidate

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	core "github.com/nostalume/mountweave"
)

func TestPublishedBaselinePreservesCandidateConfiguration(t *testing.T) {
	root, _ := json.Marshal(t.TempDir())
	cfg := filepath.Join(t.TempDir(), "config.json")
	record := `{"protocol":"webdav","root":"/files","listen":"127.0.0.1:8787","access":"read_write",
		"create_only":true,"auth":{"username":"fixture","credential":{"kind":"env","ref":"MWEAVE_TEST_UNAVAILABLE_EXPORT"}}}`
	original := fmt.Sprintf(`{"version":1,"mounts":[
		{"at":"/files","driver":"local","options":{"root":%s}},
		{"at":"/ali","driver":"alipan","connection":"website","options":{}},
		{"at":"/mobile","driver":"mobilepan","options":{}}
	],"exports":[%s]}`, root, record)
	if err := os.WriteFile(cfg, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	invokeCLI(t, cfg, "", false, 0, "mount", "list", "--json")
	invokeCLI(t, cfg, "", false, 0, "ls", "/files", "--json")
	for _, address := range []string{"/ali", "/mobile"} {
		got := invokeCLI(t, cfg, "", false, 1, "ls", address, "--json")
		if got.Error == nil || got.Error.Code != string(core.MountUnavailable) {
			t.Fatalf("candidate source at %s did not report build availability: %+v", address, got.Error)
		}
	}
	got := invokeCLI(t, cfg, "", false, 1, "serve", "--json")
	if got.Error == nil || got.Error.Code != string(core.UnsupportedOperation) {
		t.Fatalf("create-only export did not fail before accessing its credential: %+v", got.Error)
	}
	now, err := os.ReadFile(cfg)
	if err != nil || string(now) != original {
		t.Fatalf("failed operation did not preserve the configuration: %v", err)
	}
}
