// checkin.go /admin/api/checkin：面板手动触发签到（写操作，需 TW2A_API_KEY）。
//
// 复用 upstream.Checkin（status → claim + 9074 退避重试），签到后顺带刷新积分并解冻冷却账号。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"trae2api-web/internal/pool"
)

// checkinEntry 单账号签到结果。
type checkinEntry struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	Status    string `json:"status"` // ok | already | disabled | fail
	Detail    string `json:"detail,omitempty"`
	Remain    int64  `json:"remain"`
	HasRemain bool   `json:"has_remain"`
}

// adminCheckin POST /admin/api/checkin：对所有账号（或 body.uid 指定单个）执行签到。
// body 可空（视为全部）；{"uid":"1958..."} 只签指定账号。
func (h *Handler) adminCheckin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	if body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)); len(strings.TrimSpace(string(body))) > 0 {
		_ = json.Unmarshal(body, &req) // 解析失败按全量处理
	}

	targets := h.cfg.Pool.List()
	if req.UID != "" {
		filtered := targets[:0:0]
		for _, s := range targets {
			if s.UID == req.UID {
				filtered = append(filtered, s)
			}
		}
		targets = filtered
		if len(targets) == 0 {
			writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
	}

	out := make([]checkinEntry, len(targets))
	var wg sync.WaitGroup
	for i, st := range targets {
		wg.Add(1)
		go func(i int, st pool.Status) {
			defer wg.Done()
			e := checkinEntry{UID: st.UID, Nickname: st.Nickname, Status: "fail"}
			// session 失效的账号签不了，跳过（与调度器一致）。
			if st.Disabled {
				e.Status = "disabled"
				e.Detail = "session dead"
				out[i] = e
				return
			}
			a := h.cfg.Pool.AuthByUID(st.UID)
			if a == nil {
				e.Detail = "no auth found"
				out[i] = e
				return
			}
			res, err := h.cfg.Upstream.Checkin(a)
			if err != nil {
				e.Status = "fail"
				e.Detail = err.Error()
			} else {
				e.Status = res.String()
			}
			// 签到后刷新积分 + 解冻（ReenableIfCredits 同时回写池内 credits）
			if remain, qerr := h.cfg.Upstream.UserEntUsage(a); qerr == nil {
				e.Remain, e.HasRemain = remain, true
				h.cfg.Pool.ReenableIfCredits(st.UID, remain)
			}
			out[i] = e
		}(i, st)
	}
	wg.Wait()

	var okN, alreadyN, disabledN, failN int
	for _, e := range out {
		switch e.Status {
		case "ok":
			okN++
		case "already":
			alreadyN++
		case "disabled":
			disabledN++
		default:
			failN++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": out,
		"summary": map[string]int{
			"total":    len(out),
			"ok":       okN,
			"already":  alreadyN,
			"disabled": disabledN,
			"fail":     failN,
		},
	})
}
