// version.go /admin/api/version：返回构建版本信息，用于确认部署是否已更新。
package server

import (
	"net/http"
	"runtime"
	"time"

	"trae2api-web/internal/version"
)

// adminVersion GET /admin/api/version
//
// 只读、无鉴权（与其它 admin 只读接口一致）。典型用途：部署后 `curl /admin/api/version`
// 看 short_commit 是否等于 `git rev-parse --short HEAD`；页面角标也读这里。
func (h *Handler) adminVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           version.ID(),
		"version":      version.Version,
		"commit":       version.Commit,
		"short_commit": version.ShortCommit(),
		"commit_time":  version.BuildTime,
		"modified":     version.Modified,
		"go_version":   runtime.Version(),
		"started_at":   version.ProcessStart.Format(time.RFC3339),
		"badge":        version.Badge(),
	})
}
