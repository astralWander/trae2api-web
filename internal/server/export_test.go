package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"trae2api-web/internal/auth"
)

// 未配置 APIKey 时导出接口必须 403——否则等于把全部 token 裸奔在公网。
func TestExportRequiresAPIKey(t *testing.T) {
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "secret-token"}),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/accounts/export", nil))
	if rec.Code != 403 {
		t.Fatalf("code=%d want 403 body=%s", rec.Code, rec.Body)
	}
}

// 配了 Key 但 Bearer 不对 → 401（沿用 withAdminAuth）。
func TestExportWrongKey401(t *testing.T) {
	h := NewHandler(Config{
		Pool:   testPoolWith(&auth.Auth{UID: "u1", AccessToken: "secret-token"}),
		APIKey: "k",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/api/accounts/export", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}

// 正确鉴权后必须返回**未脱敏**的完整凭证，且能被 auth.Parse 原样解开。
func TestExportReturnsFullCredentials(t *testing.T) {
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{
			UID: "u1", Nickname: "n1", AccessToken: "secret-token", RefreshToken: "rt-1",
			DeviceID: "1711320556112436", MarketUserID: "mkt-1", EnterpriseID: "ent",
		}),
		APIKey: "k",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/api/accounts/export", nil)
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var resp struct {
		Count    int `json:"count"`
		Accounts []struct {
			UID         string          `json:"uid"`
			Credentials json.RawMessage `json:"credentials"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v body=%s", err, rec.Body)
	}
	if resp.Count != 1 || len(resp.Accounts) != 1 {
		t.Fatalf("count=%d accounts=%d", resp.Count, len(resp.Accounts))
	}
	a, err := auth.Parse(resp.Accounts[0].Credentials)
	if err != nil {
		t.Fatalf("导出的凭证必须可被 auth.Parse 解析: %v", err)
	}
	if a.JWT() != "secret-token" || a.RefreshTokenValue() != "rt-1" {
		t.Fatalf("凭证未完整导出: jwt=%q rt=%q", a.JWT(), a.RefreshTokenValue())
	}
	if a.UID != "u1" || a.Nickname != "n1" || a.DeviceIDValue() != "1711320556112436" {
		t.Fatalf("account/device 字段丢失: %+v", a)
	}
}

// ?uid= 只导出指定账号。
func TestExportUIDFilter(t *testing.T) {
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1"},
			&auth.Auth{UID: "u2", AccessToken: "at2"},
		),
		APIKey: "k",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/api/accounts/export?uid=u2", nil)
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)
	var resp struct {
		Accounts []struct {
			UID string `json:"uid"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(resp.Accounts) != 1 || resp.Accounts[0].UID != "u2" {
		t.Fatalf("accounts=%+v want only u2", resp.Accounts)
	}
}

// 回报接口：积分回写 + 续期凭证回存（refreshToken 轮换后必须同步回服务端）。
func TestCheckinReportUpdatesCreditsAndCredentials(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "old-token"})
	dir := t.TempDir()
	h := NewHandler(Config{Pool: p, APIKey: "k", AuthDir: dir})

	cred := `{"auth":{"accessToken":"new-token","refreshToken":"rt-2","deviceId":"1711320556112436","marketUserId":"mkt"},"account":{"uid":"u1"}}`
	body := `{"results":[{"uid":"u1","status":"ok","remain":321,"has_remain":true}],"credentials":[` + cred + `]}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/api/checkin/report", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	st, ok := p.Status("u1")
	if !ok || st.Credits != 321 {
		t.Fatalf("credits=%d want 321", st.Credits)
	}
	a := p.AuthByUID("u1")
	if a == nil || a.JWT() != "new-token" {
		t.Fatalf("续期凭证未回存: %+v", a)
	}
}

// 回报接口同样受 Key 保护。
func TestCheckinReportRequiresKey(t *testing.T) {
	h := NewHandler(Config{
		Pool:   testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at"}),
		APIKey: "k",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/api/checkin/report", strings.NewReader(`{}`)))
	if rec.Code != 401 {
		t.Fatalf("code=%d want 401", rec.Code)
	}
}
