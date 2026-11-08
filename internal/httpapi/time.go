package httpapi

import (
	"net/http"
	"time"
)

func (a *API) handleTime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "不支持的 HTTP 方法")
		return
	}
	now := time.Now()
	writeJSON(w, http.StatusOK, map[string]any{
		"serverTime": now.UnixMilli(),
		"iso":        now.UTC().Format(time.RFC3339Nano),
	})
}
