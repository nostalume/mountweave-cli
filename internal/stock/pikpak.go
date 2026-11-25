package stock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	core "github.com/nostalume/mountweave"
	"github.com/nostalume/mountweave-cli/internal/cliapp"
	"github.com/nostalume/mountweave/drivers/pikpak"
)

type pikPakOptions struct {
	RootID   string `json:"root_id,omitempty"`
	DeviceID string `json:"device_id"`
}

func pikPakRoute() *cliapp.Route {
	route := cliapp.SourceRoute(func(o *pikPakOptions, ask cliapp.Prompt) error {
		if err := askClearable(ask, "PikPak folder ID (empty for account root; - resets)", &o.RootID); err != nil {
			return err
		}
		if o.DeviceID == "" {
			var id [16]byte
			if _, err := rand.Read(id[:]); err != nil {
				return err
			}
			o.DeviceID = hex.EncodeToString(id[:])
		}
		return nil
	}, func(o pikPakOptions, _ string) (cliapp.MountSource, error) {
		if !pikpak.ValidLocation(o.RootID, o.DeviceID) {
			return cliapp.MountSource{}, cliapp.InvalidConfig("PikPak needs a valid folder ID and device ID. The mount wizard generates the device ID.")
		}
		source := cliapp.BearerSource("https", func(ctx context.Context, api *http.Client) (core.Driver, error) {
			return cliapp.NativeDriver(pikpak.Open(ctx, pikpak.Options{
				RootID: o.RootID, DeviceID: o.DeviceID, Client: api, ContentClient: storageHTTPClient,
			}))
		}, pikpak.ControlHost)
		source.Secret = "PikPak read-only personal access token"
		return source, nil
	})
	return &route
}
