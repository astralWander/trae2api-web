package version

import "testing"

func TestShortCommitAndID(t *testing.T) {
	oldC, oldB, oldM := Commit, BuildTime, Modified
	defer func() { Commit, BuildTime, Modified = oldC, oldB, oldM }()

	// 无 VCS 信息 → unknown
	Commit, BuildTime, Modified = "", "", false
	if got := ShortCommit(); got != "unknown" {
		t.Errorf("ShortCommit()=%q want unknown", got)
	}
	if got := ID(); got != "unknown" {
		t.Errorf("ID()=%q want unknown", got)
	}
	if got := Badge(); got != "unknown" {
		t.Errorf("Badge()=%q want unknown", got)
	}

	// 有 commit → 7 位短哈希
	Commit, Modified = "abcdef0123456789", false
	if got := ShortCommit(); got != "abcdef0" {
		t.Errorf("ShortCommit()=%q want abcdef0", got)
	}
	if got := ID(); got != "abcdef0" {
		t.Errorf("ID()=%q want abcdef0", got)
	}

	// 工作区有改动 → -dirty 后缀
	Modified = true
	if got := ID(); got != "abcdef0-dirty" {
		t.Errorf("ID()=%q want abcdef0-dirty", got)
	}

	// Badge 拼版本 + 提交时间（RFC3339 → 本地 MM-DD HH:MM）
	Version = "v1.2.3"
	BuildTime = "2026-10-08T09:03:00Z"
	if got := Badge(); got != "v1.2.3 · abcdef0-dirty · 10-08 17:03" {
		t.Errorf("Badge()=%q", got)
	}
	Version = ""
}

// 非法时间串原样保留，不 panic。
func TestBadgeBadTime(t *testing.T) {
	oldC, oldB, oldM := Commit, BuildTime, Modified
	oldV := Version
	defer func() { Commit, BuildTime, Modified, Version = oldC, oldB, oldM, oldV }()

	Commit, BuildTime, Modified, Version = "abcdef0123456789", "not-a-time", false, ""
	if got := Badge(); got != "abcdef0 · not-a-time" {
		t.Errorf("Badge()=%q", got)
	}
}
