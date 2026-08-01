package migrate

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// migrationFileName 釘住檔名格式：五位數版號 + 底線 + 小寫描述 + .sql。
//
// 版號補到五位是為了讓「字典序」與「數值序」永遠一致——goose 是照數值排的，
// 但人是照檔案總管的字典序看的，兩者不一致時 code review 會看漏順序問題。
var migrationFileName = regexp.MustCompile(`^(\d{5})_[a-z0-9_]+\.sql$`)

// TestMigrationFilesAreWellFormed 釘住 migration 檔的四條規則。
//
// ⭐ 第二條（版號不得重複）直接來自 docs/ADR-001 決策 5：團隊的 PostgreSQL
// 那邊有**兩個 `V15__` 同版號並存**，重放時的執行順序取決於檔名排序，
// 也就是不確定的。這種錯不會有錯誤訊息，只會有「在我這台跑起來是對的」。
func TestMigrationFilesAreWellFormed(t *testing.T) {
	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		t.Fatalf("讀取內嵌 migration 目錄失敗: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("一個 migration 都沒有——embed 是不是沒抓到檔案？")
	}

	seen := make(map[int64]string, len(entries))
	var previous int64

	for _, entry := range entries {
		name := entry.Name()

		matches := migrationFileName.FindStringSubmatch(name)
		if matches == nil {
			t.Errorf("%s 不符合檔名格式 NNNNN_描述.sql", name)
			continue
		}
		version, err := strconv.ParseInt(matches[1], 10, 64)
		if err != nil {
			t.Errorf("%s 的版號解析失敗: %v", name, err)
			continue
		}

		if other, duplicated := seen[version]; duplicated {
			t.Errorf("版號 %05d 重複：%s 與 %s——重放順序會變成不確定（ADR-001 決策 5）",
				version, other, name)
		}
		seen[version] = name

		// fs.ReadDir 回傳的是字典序，配上固定寬度的版號等於數值序。
		if version <= previous {
			t.Errorf("%s 的版號沒有遞增（前一個是 %05d）", name, previous)
		}
		previous = version

		content, err := fs.ReadFile(migrationsFS, migrationsDir+"/"+name)
		if err != nil {
			t.Errorf("讀取 %s 失敗: %v", name, err)
			continue
		}
		up, down, split := strings.Cut(string(content), "-- +goose Down")
		if !strings.Contains(up, "-- +goose Up") {
			t.Errorf("%s 缺少 `-- +goose Up` 標記，goose 會當成空的 migration", name)
		}
		if !split || strings.TrimSpace(down) == "" {
			t.Errorf("%s 缺少 `-- +goose Down`——少了它，「這個版本能不能退回去」只能靠猜", name)
		}

		// ⚠️ 只有 baseline（00001）可以用 IF NOT EXISTS：它要能在
		// initdb.d 時代就已建好表的既有 volume 上安全地補記版本。
		// 之後的 migration 用 IF NOT EXISTS 等於把「這個變更沒生效」
		// 靜靜吞掉——而 migration 工具存在的意義就是不讓這件事發生。
		if version > 1 && strings.Contains(strings.ToUpper(up), "IF NOT EXISTS") {
			t.Errorf("%s 的 Up 段使用了 IF NOT EXISTS——只有 baseline 可以這樣寫，"+
				"其餘的會把「變更沒生效」靜靜吞掉", name)
		}
	}
}
