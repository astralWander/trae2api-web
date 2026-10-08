// report.go POST /admin/api/checkin/report：本地签到程序把结果回报给服务端。
//
// 为什么需要回报：签到在**本机**发出，服务端感知不到，于是
//  1. 池内 credits 不会更新（面板与挑选策略用的是这个缓存值）；
//  2. 本地 refresh 会轮换 refreshToken —— 若不回写，服务端手里那份旧
//     refreshToken 会失效，之后服务端自己续期就会 session dead。
//
// 本接口一次解决这两件事。
package server

import (
	"encoding/json"
	"net/http"

	"trae2api-web/internal/auth"
)

// reportResult 单账号签到结果（与面板 /admin/api/checkin 的 checkinEntry 同构）。
type reportResult struct {
	UID       string `json:"uid"`
	Status    string `json:"status"` // ok | already | disabled | fail
	Detail    string `json:"detail,omitempty"`
	Remain    int64  `json:"remain"`
	HasRemain bool   `json:"has_remain"`
}

// reportRequest 回报体。credentials 可选：仅本地发生过 refresh / 身份迁移的账号需回传。
type reportRequest struct {
	Results     []reportResult    `json:"results"`
	Credentials []json.RawMessage `json:"credentials,omitempty"`
}

// adminCheckinReport POST /admin/api/checkin/report（写操作，需 Key）。
func (h *Handler) adminCheckinReport(w http.ResponseWriter, r *http.Request) {
	var req reportRequest
	if err := decodeBody(w, r, &req); err != nil {
		return
	}

	creditsUpdated := 0
	for _, res := range req.Results {
		if res.UID == "" || !res.HasRemain {
			continue
		}
		// 同时回写积分并按「remain>0」解冻冷却账号（与面板签到路径一致）。
		h.cfg.Pool.ReenableIfCredits(res.UID, res.Remain)
		creditsUpdated++
	}

	credsSaved := 0
	for _, raw := range req.Credentials {
		a, err := auth.Parse(raw)
		if err != nil || a.UID == "" {
			continue
		}
		if h.cfg.AuthDir == "" {
			continue
		}
		a.FilePath = auth.FilePathFor(h.cfg.AuthDir, a.UID)
		if err := a.SaveAtomic(); err != nil {
			continue
		}
		h.cfg.Pool.Add(a) // 已存在则保留 cooling/enabled 状态，仅更新凭证
		credsSaved++
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"credits_updated":   creditsUpdated,
		"credentials_saved": credsSaved,
	})
}
