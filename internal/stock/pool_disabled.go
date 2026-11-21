//go:build !mweave_pool

package stock

import "github.com/nostalume/mountweave-cli/internal/cliapp"

func poolRoute() cliapp.Route { return cliapp.Route{} }
