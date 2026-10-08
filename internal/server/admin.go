// 管理面板（/admin）：只读查询，无鉴权（本地面板）；CLI 操作留待开发。
package server

import (
	"bytes"
	_ "embed"
	"net/http"
	"sync"
	"time"

	"trae2api-web/internal/pool"
	"trae2api-web/internal/version"
)

//go:embed admin.html
var adminPageHTML []byte

// buildPlaceholder admin.html 里的构建标识占位符，服务时替换为 version.Badge()。
// 这样页面源码（curl /admin 即可）直接带版本号，无需 JS/接口也能核对是否拉取成功。
var buildPlaceholder = []byte("__TW2A_BUILD__")

// adminPage 返回内嵌 HTML 面板（深色简洁风，无外部依赖）。
func (h *Handler) adminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(renderAdminPage())
}

// renderAdminPage 把构建标识注入内嵌 HTML 的占位符。
func renderAdminPage() []byte {
	return bytes.ReplaceAll(adminPageHTML, buildPlaceholder, []byte(version.Badge()))
}

// adminCredits 查询全部账号的实时额度 + 签到状态（并发拉取上游）。
func (h *Handler) adminCredits(w http.ResponseWriter, r *http.Request) {
	type acct struct {
		UID            string `json:"uid"`
		Nickname       string `json:"nickname"`
		Remain         int64  `json:"remain"`
		Limit          int64  `json:"limit"`
		Used           int64  `json:"used"`
		Packs          int    `json:"packs"`
		CheckedIn      bool   `json:"checked_in"`
		CheckinCredits int64  `json:"checkin_credits"`
		CheckinEnable  bool   `json:"checkin_enable"`
		Cooling        bool   `json:"cooling"`
		Disabled       bool   `json:"disabled"`
		Error          string `json:"error,omitempty"`
	}

	st := h.cfg.Pool.List()
	out := make([]acct, len(st))
	var wg sync.WaitGroup
	for i, s := range st {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			a := h.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				out[i] = acct{UID: s.UID, Nickname: s.Nickname, Error: "no auth found"}
				return
			}
			var ac acct
			ac.UID = s.UID
			ac.Nickname = s.Nickname
			ac.Cooling = s.Cooling
			ac.Disabled = s.Disabled
			remain, limit, used, packs, err := h.cfg.Upstream.EntUsage(a)
			if err != nil {
				ac.Error = "ent_usage: " + err.Error()
			} else {
				ac.Remain, ac.Limit, ac.Used, ac.Packs = remain, limit, used, packs
			}
			checkedIn, credits, enable, cerr := h.cfg.Upstream.CheckinStatus(a)
			if cerr != nil {
				if ac.Error != "" {
					ac.Error += "; "
				}
				ac.Error += "checkin: " + cerr.Error()
			} else {
				ac.CheckedIn, ac.CheckinCredits, ac.CheckinEnable = checkedIn, credits, enable
			}
			out[i] = ac
		}(i, s)
	}
	wg.Wait()

	// 全账号合计（只统计查询成功的账号，失败的不计入，避免把 error 当 0 拉低总数）。
	var totRemain, totLimit, totUsed int64
	var okN, errN, pendingN int
	for _, a := range out {
		if a.Error != "" {
			errN++
			continue
		}
		okN++
		totRemain += a.Remain
		totLimit += a.Limit
		totUsed += a.Used
		if !a.CheckedIn && a.CheckinEnable {
			pendingN++ // 今日尚未签到
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"fetched_at": time.Now().Format("2006-01-02 15:04:05"),
		"accounts":   out,
		"total": map[string]any{
			"remain":      totRemain,
			"limit":       totLimit,
			"used":        totUsed,
			"accounts":    len(out),
			"ok":          okN,
			"error":       errN,
			"checkin_pending": pendingN,
		},
	})
}
