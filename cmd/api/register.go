package api

import (
	"net/http"

	"github.com/Seraphli/tg-cli/cmd/types"
)

// Register registers all HTTP API endpoints.
func Register(mux *http.ServeMux, bs *types.BotState) {
	registerPagination(mux, bs)
	registerPermission(mux, bs)
	registerTool(mux, bs)
	registerRouting(mux, bs)
	registerInjection(mux, bs)
	registerSession(mux, bs)
	registerLaunch(mux, bs)
	registerResume(mux, bs)
	registerMerge(mux, bs)
	registerCron(mux, bs)
	registerTmux(mux, bs)
	registerFile(mux, bs)
	registerAt(mux, bs)
	RegisterTestEndpoints(mux, bs)
}
