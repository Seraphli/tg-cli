package cmd

import (
	"encoding/json"
	"net/http"
	"os"
	"time"
)

// registerHealth registers the lightweight /health endpoint. It does no session scanning:
// it just reports liveness + build identity (version, pid, uptime, binary md5) so the upgrade
// flow and external monitors can confirm the running bot. Lives in the cmd package because it
// reads Version, which the api package cannot import (cmd -> api import cycle).
func registerHealth(mux *http.ServeMux, startTime time.Time, binaryMD5 string) {
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":             true,
			"version":        Version,
			"pid":            os.Getpid(),
			"uptime_seconds": int(time.Since(startTime).Seconds()),
			"binary_md5":     binaryMD5,
		})
	})
}
