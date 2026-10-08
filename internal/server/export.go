// export.go GET /admin/api/accounts/export：把完整账号凭证导出给本地签到程序。
//
// 背景：机房 / 云服务器出口 IP 会被上游风控限制签到（status 说可领、claim 恒 9074）。
// 对策是把「签到」这一跳挪到本机（家宽 / 办公网）执行，服务器只当账号仓库。
// 本接口就是仓库的取货口。
//
// 安全纪律（与 accounts.go 的脱敏纪律相反，故单独成文件强调）：
//   - 本接口返回**未脱敏**的完整 accessToken / refreshToken，因为本地程序需要
//     真实凭证直连上游。因此放行条件比其余接口更严：
//   - 未配置 TW2A_API_KEY → 直接 403（否则等于把全部 token 裸奔在公网）
//   - 必须携带正确的 Bearer（withAdminAuth）
//   - 只读，不落盘、不改状态
package server

import (
	"net/http"
	"time"
)

// exportAccount 单账号导出项。
type exportAccount struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	// Disabled = session 失效硬禁用（本地签不了，可跳过）；Enabled = 用户软开关。
	Disabled  bool  `json:"disabled"`
	Enabled   bool  `json:"enabled"`
	Cooling   bool  `json:"cooling,omitempty"`
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// Credentials 嵌套形 {"auth":{...},"account":{...}}，与磁盘 trae-*.json 同构，
	// 本地程序 json.Marshal 后即可 auth.Parse。
	Credentials map[string]any `json:"credentials"`
}

// adminExportAccounts GET /admin/api/accounts/export
//
// 查询参数：
//
//	uid=...              只导出指定账号（默认全部）
//	include_disabled=0   排除 session 失效账号（默认包含，本地程序自行跳过）
func (h *Handler) adminExportAccounts(w http.ResponseWriter, r *http.Request) {
	if h.cfg.APIKey == "" {
		writeOpenAIError(w, http.StatusForbidden, "api_key_required",
			"export returns raw tokens; set TW2A_API_KEY before using this endpoint")
		return
	}
	wantUID := r.URL.Query().Get("uid")
	includeDisabled := r.URL.Query().Get("include_disabled") != "0"

	out := make([]exportAccount, 0, 8)
	for _, s := range h.cfg.Pool.List() {
		if wantUID != "" && s.UID != wantUID {
			continue
		}
		if s.Disabled && !includeDisabled {
			continue
		}
		a := h.cfg.Pool.AuthByUID(s.UID)
		if a == nil {
			continue
		}
		out = append(out, exportAccount{
			UID:         s.UID,
			Nickname:    s.Nickname,
			Disabled:    s.Disabled,
			Enabled:     s.Enabled,
			Cooling:     s.Cooling,
			ExpiresAt:   a.ExpiresAt,
			Credentials: a.CredentialsDoc(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exported_at": time.Now().Format(time.RFC3339),
		"count":       len(out),
		"accounts":    out,
	})
}
