// localcheckin 本地签到程序：把「签到」这一跳挪到本机出口 IP 执行。
//
// 为什么需要它：服务器（机房/云厂商 IP 段）会被上游风控盯上 —— 表现为
// status 明确说「可领 100」，claim 却恒定 9074「当前参与用户太多」。
// 换设备号、换 UA、退避重试都无效，因为限流维度是**出口 IP**。
// 对策：账号仍存在服务器上，签到改由本机（家宽/办公网）发出。
//
// 工作流：
//
//	本机 ──GET /admin/api/accounts/export──▶ 服务器（取账号，未脱敏）
//	本机 ──POST upstream checkin/claim───▶ TRAE（用本机 IP 签到）
//	本机 ──POST /admin/api/checkin/report─▶ 服务器（回报结果 + 续期后的凭证）
//
// 用法：
//
//	localcheckin -server http://1.2.3.4:7864 -key <TW2A_API_KEY>
//	localcheckin -dry-run            # 只拉取并打印账号清单（验证是否拉取成功）
//	localcheckin -uid 1958679338298644
//
// 环境变量：TW2A_SERVER、TW2A_API_KEY（可省 --server/--key）
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/upstream"
)

// exportAccount 服务端导出接口的账号项（与服务端 export.go 对应）。
type exportAccount struct {
	UID         string          `json:"uid"`
	Nickname    string          `json:"nickname"`
	Disabled    bool            `json:"disabled"`
	Enabled     bool            `json:"enabled"`
	Cooling     bool            `json:"cooling"`
	ExpiresAt   int64           `json:"expires_at"`
	Credentials json.RawMessage `json:"credentials"` // 嵌套形，可直接 auth.Parse
}

type exportResp struct {
	ExportedAt string          `json:"exported_at"`
	Count      int             `json:"count"`
	Accounts   []exportAccount `json:"accounts"`
}

// row 单账号执行结果。
type row struct {
	uid    string
	nick   string
	status string // OK | ALREADY | DISABLED | FAIL | AUTH_INVALID | LOAD_ERR
	detail string
	remain int64
	hasRem bool
	dirty  bool            // 凭证需要在服务端回写
	cred   json.RawMessage // 回写用的嵌套凭证
}

func main() {
	server := flag.String("server", envOr("TW2A_SERVER", "http://127.0.0.1:7864"), "服务端地址（含端口）")
	key := flag.String("key", os.Getenv("TW2A_API_KEY"), "服务端 API Key（Bearer），或用 TW2A_API_KEY")
	uid := flag.String("uid", "", "只处理指定账号 uid（默认全部）")
	dryRun := flag.Bool("dry-run", false, "只拉取并打印账号清单，不执行签到（验证是否拉取成功）")
	report := flag.Bool("report", true, "把签到结果与续期凭证回报服务端（-report=false 关闭）")
	saveDir := flag.String("save", "", "把拉到的账号落盘到该目录（默认不落盘，仅内存）")
	all := flag.Bool("all", false, "包含 session 失效（disabled）的账号（默认跳过）")
	concurrency := flag.Int("concurrency", 1, "并发账号数，默认 1（顺序执行最不易撞上游限流）")
	timeout := flag.Int("timeout", 30, "对服务端请求的超时秒数")
	flag.Parse()

	if *key == "" {
		fatal("缺少 API Key：用 -key 或环境变量 TW2A_API_KEY")
	}
	base := strings.TrimRight(*server, "/")

	accounts, err := fetchExport(base, *key, *uid, *timeout)
	if err != nil {
		fatal("拉取账号失败：" + err.Error())
	}
	if len(accounts) == 0 {
		fmt.Println("服务端没有任何账号（先导入凭证）。")
		return
	}

	// 默认跳过 session 失效账号（本地也签不了，只会浪费一次请求）。
	if !*all {
		kept := accounts[:0:0]
		for _, a := range accounts {
			if !a.Disabled {
				kept = append(kept, a)
			}
		}
		accounts = kept
	}
	if len(accounts) == 0 {
		fmt.Println("没有可处理的账号（全部 session 失效；用 -all 可强制包含）。")
		return
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].UID < accounts[j].UID })

	fmt.Printf("已从 %s 拉取 %d 个账号", base, len(accounts))
	if *dryRun {
		fmt.Println("（dry-run，仅展示）：")
		printPulled(accounts)
		return
	}
	fmt.Println("，开始在本机签到…")

	if *saveDir != "" {
		if err := os.MkdirAll(*saveDir, 0o700); err != nil {
			fatal("创建本地目录失败：" + err.Error())
		}
	}

	up := upstream.New()
	rows := make([]row, len(accounts))

	sem := make(chan struct{}, maxInt(1, *concurrency))
	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			rows[i] = checkOne(up, accounts[i], *saveDir)
		}(i)
	}
	wg.Wait()

	if *report {
		if err := postReport(base, *key, rows, *timeout); err != nil {
			fmt.Fprintf(os.Stderr, "⚠ 回报服务端失败（签到已生效，仅面板数据未刷新）：%v\n", err)
		} else {
			fmt.Println("已回报服务端（积分已刷新）。")
		}
	}

	printRows(rows)
}

// checkOne 处理单个账号：解析 → 补身份 → 续期 → 签到 → 查积分。
func checkOne(up *upstream.Client, acc exportAccount, saveDir string) row {
	r := row{uid: acc.UID, nick: acc.Nickname, status: "FAIL"}

	a, err := auth.Parse(acc.Credentials)
	if err != nil {
		r.status, r.detail = "LOAD_ERR", "parse: "+short(err.Error())
		return r
	}
	if saveDir != "" {
		a.FilePath = filepath.Join(saveDir, "trae-"+a.UID+".json")
	}

	// 身份迁移：deviceId 必须是 15~16 位纯数字，marketUserId 必须存在。
	// 两者任一缺失/形态不对都会导致签到失败（9004 / 9074），且需要回写服务端。
	dirty := a.EnsureDeviceID()
	if a.EnsureMarketUserID() {
		dirty = true
	}

	// access token 临近过期 → 本机续期（会轮换 refreshToken，必须回写服务端）。
	if a.NeedsRefresh(2 * time.Hour) {
		if err := up.RefreshToken(a); err != nil {
			r.status = "AUTH_INVALID"
			r.detail = "refresh: " + short(err.Error())
			return r
		}
		dirty = true
	}

	res, err := up.Checkin(a)
	switch {
	case err != nil:
		if isAlready(err.Error()) {
			r.status, r.detail = "ALREADY", short(err.Error())
		} else {
			r.status, r.detail = "FAIL", short(err.Error())
		}
	case res == upstream.CheckinAlready:
		r.status, r.detail = "ALREADY", "already checked in"
	case res == upstream.CheckinDisabled:
		r.status, r.detail = "DISABLED", "checkin disabled"
	default:
		r.status = "OK"
	}

	if dirty {
		if b, merr := json.Marshal(a.CredentialsDoc()); merr == nil {
			r.cred, r.dirty = b, true
		}
	}
	if saveDir != "" && a.FilePath != "" {
		_ = a.SaveAtomic()
	}
	if remain, qerr := up.UserEntUsage(a); qerr == nil {
		r.remain, r.hasRem = remain, true
	}
	return r
}

// fetchExport GET /admin/api/accounts/export。
func fetchExport(base, key, uid string, timeout int) ([]exportAccount, error) {
	u := base + "/admin/api/accounts/export"
	if uid != "" {
		u += "?" + url.Values{"uid": {uid}}.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)

	cl := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, errors.New("鉴权失败（401）：Key 不对")
	case http.StatusForbidden:
		return nil, errors.New("服务端拒绝了导出（403）：请先在服务端配置 TW2A_API_KEY")
	default:
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, trunc(string(body), 200))
	}

	var out exportResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("响应不是预期的 JSON（%w）：%s", err, trunc(string(body), 120))
	}
	return out.Accounts, nil
}

// reportResult / reportReq 与 service 端 report.go 对应。
type reportResult struct {
	UID       string `json:"uid"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Remain    int64  `json:"remain"`
	HasRemain bool   `json:"has_remain"`
}

type reportReq struct {
	Results     []reportResult    `json:"results"`
	Credentials []json.RawMessage `json:"credentials,omitempty"`
}

// postReport POST /admin/api/checkin/report。
func postReport(base, key string, rows []row, timeout int) error {
	req := reportReq{Results: make([]reportResult, 0, len(rows))}
	for _, r := range rows {
		req.Results = append(req.Results, reportResult{
			UID: r.uid, Status: strings.ToLower(r.status), Detail: r.detail,
			Remain: r.remain, HasRemain: r.hasRem,
		})
		if r.dirty && len(r.cred) > 0 {
			req.Credentials = append(req.Credentials, r.cred)
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequest(http.MethodPost, base+"/admin/api/checkin/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+key)

	cl := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := cl.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, trunc(string(respBody), 200))
	}
	return nil
}

// printPulled dry-run 输出：账号清单 + 关键身份字段体检。
func printPulled(accounts []exportAccount) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "uid\tnickname\tdeviceId\tmarketUserId\tauthToken\texpires\tdisabled")
	for _, acc := range accounts {
		dev, mkt, hasTok := "-", "-", "no"
		if a, err := auth.Parse(acc.Credentials); err == nil {
			dev = a.DeviceIDValue()
			if dev == "" {
				dev = "(空)"
			} else if !auth.IsRealDeviceID(dev) {
				dev += " ✗形态"
			}
			mkt = a.MarketUserIDValue()
			if mkt == "" {
				mkt = "(空)"
			}
			if a.JWT() != "" {
				hasTok = "yes"
			}
		}
		exp := "-"
		if acc.ExpiresAt > 0 {
			exp = time.Unix(acc.ExpiresAt, 0).Format("01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%v\n",
			acc.UID, orDash(acc.Nickname), dev, mkt, hasTok, exp, acc.Disabled)
	}
	tw.Flush()
}

// printRows 结果表 + 汇总。
func printRows(rows []row) {
	var okN, alreadyN, disabledN, failN int
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "\nuid\tnickname\tstatus\tremain\tdetail")
	fmt.Fprintln(tw, "---\t--------\t------\t------\t------")
	for _, r := range rows {
		remain := "-"
		if r.hasRem {
			remain = fmt.Sprintf("%d", r.remain)
		}
		switch r.status {
		case "OK":
			okN++
		case "ALREADY":
			alreadyN++
		case "DISABLED":
			disabledN++
		default:
			failN++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.uid, orDash(r.nick), r.status, remain, r.detail)
	}
	tw.Flush()
	fmt.Printf("\n签到完成：成功 %d · 已签 %d · 未开放 %d · 失败 %d\n", okN, alreadyN, disabledN, failN)
}

// ---------------------------------------------------------------------------

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func short(s string) string { return trunc(s, 70) }

// isAlready 已签判定：仅匹配明确表示"今日已签到"的业务错误。
func isAlready(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already check") ||
		strings.Contains(s, "already checked")
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "错误："+msg)
	os.Exit(1)
}
