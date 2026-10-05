package housekeeping

import (
	"github.com/ajaykathwatee/previa/internal/builds"
	"github.com/ajaykathwatee/previa/internal/environments"
	"github.com/ajaykathwatee/previa/internal/pullrequests"
	"github.com/ajaykathwatee/previa/internal/routing"
	"github.com/ajaykathwatee/previa/internal/runtime"
	"github.com/ajaykathwatee/previa/internal/source"
)

// The production adapters satisfy what housekeeping asks of them.
var (
	_ Scheduling = (*environments.Broker)(nil)
	_ Operations = (*environments.Repository)(nil)
	_ Teardowns  = (*environments.Repository)(nil)
	_ Sweeper    = (*environments.TeardownSweeper)(nil)
	_ RouteSync  = (*routing.Controller)(nil)
	_ Verifier   = (*pullrequests.SyncService)(nil)
	_ Leftovers  = (*runtime.Runtime)(nil)
	_ Caches     = (*builds.Builder)(nil)
	_ Workspaces = (*source.Workspaces)(nil)
)
