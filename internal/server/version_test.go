package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"trae2api-web/internal/version"
)

// 面板必须把构建标识注入占位符：页面源码可直接看到版本，用于确认服务器是否已更新。
func TestRenderAdminPageInjectsBuild(t *testing.T) {
	page := string(renderAdminPage())
	if strings.Contains(page, string(buildPlaceholder)) {
		t.Fatalf("placeholder %q 未被替换", buildPlaceholder)
	}
	if !strings.Contains(page, version.ID()) {
		t.Fatalf("页面缺少构建标识 %q", version.ID())
	}
}

// /admin/api/version 返回构建信息，字段应与 version 包一致。
func TestAdminVersionEndpoint(t *testing.T) {
	h := &Handler{}
	rr := httptest.NewRecorder()
	h.adminVersion(rr, httptest.NewRequest("GET", "/admin/api/version", nil))
	if rr.Code != 200 {
		t.Fatalf("code=%d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got["short_commit"] != version.ShortCommit() {
		t.Fatalf("short_commit=%v want %v", got["short_commit"], version.ShortCommit())
	}
	if got["id"] != version.ID() {
		t.Fatalf("id=%v want %v", got["id"], version.ID())
	}
	if _, ok := got["started_at"]; !ok {
		t.Fatal("missing started_at")
	}
}
