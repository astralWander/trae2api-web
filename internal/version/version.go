// Package version 暴露构建版本信息，用于确认「服务器是否已拉取并重启到最新代码」。
//
// 取值优先级：
//  1. 构建时注入：`-ldflags "-X trae2api-web/internal/version.Commit=..."`（CI/发布用）
//  2. go build 自动嵌入的 VCS 信息（vcs.revision / vcs.time）：在 git 仓库内直接
//     `go build ./cmd/server` 即带，无需额外参数（Go 默认 -buildvcs=auto）
//  3. 都拿不到时兜底为 "unknown"（例如在仓库外构建、或 -buildvcs=false）
//
// 面板角标 / `/admin/api/version` / 启动日志都读这里，三处应显示同一个 ID。
package version

import (
	"runtime/debug"
	"strings"
	"time"
)

// 可由 -ldflags -X 覆盖。
var (
	// Version 语义版本或发布标签（可空）。
	Version = ""
	// Commit git 完整哈希（可空，空则回退 VCS 信息）。
	Commit = ""
	// BuildTime 构建时间（可空，空则回退 VCS 提交时间）。
	BuildTime = ""
)

// Modified 构建时工作区是否有未提交改动（来自 VCS 信息；ldflags 注入时不改）。
var Modified bool

// ProcessStart 进程启动时间，用于确认服务是否重启过。
var ProcessStart = time.Now()

func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if Commit == "" {
				Commit = s.Value
			}
		case "vcs.time":
			if BuildTime == "" {
				BuildTime = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				Modified = true
			}
		}
	}
}

// ShortCommit 返回 7 位短哈希；无 VCS 信息时返回 "unknown"。
func ShortCommit() string {
	if Commit == "" {
		return "unknown"
	}
	if len(Commit) > 7 {
		return Commit[:7]
	}
	return Commit
}

// ID 一行式短标识（短哈希 + 可选 -dirty），例如 "a4e0f00" / "a4e0f00-dirty"。
// 便于直接与 `git rev-parse --short HEAD` 对照，或在页面源码里 grep。
func ID() string {
	id := ShortCommit()
	if Modified && Commit != "" {
		id += "-dirty"
	}
	return id
}

// Badge 人类可读角标，例如 "a4e0f00 · 10-08 17:03"。
// 提交时间取 VCS 的 vcs.time（RFC3339），解析失败则原样附带。
func Badge() string {
	parts := []string{ID()}
	if Version != "" {
		parts = append([]string{Version}, parts...)
	}
	if BuildTime != "" {
		parts = append(parts, humanTime(BuildTime))
	}
	return strings.Join(parts, " · ")
}

// humanTime 把 RFC3339 时间转成 "01-02 15:04"；失败则原样返回。
func humanTime(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("01-02 15:04")
	}
	return s
}
