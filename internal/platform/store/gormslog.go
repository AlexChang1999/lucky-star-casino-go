package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// ⭐ gormSlog 把 GORM 產生的 SQL 導進 `log/slog`。
//
// 為什麼需要它（藍圖 §3.2）：帳務正確性建立在兩件事上——**冪等鍵的 UNIQUE 衝突**
// 與**樂觀鎖比對之後的受影響列數**。GORM 兩件都做得到，問題在它預設把產生的 SQL
// 藏起來，而那正是最容易出錯的地方。面試時這是好答案：不是「我不敢用 ORM」，
// 而是**「我知道 ORM 在哪裡會騙我」**。
//
// 為什麼不直接用 `db.Debug()`：那是**全域**開關，而且輸出是給人看的純文字。
// 本專案的 log 一律走 slog 的結構化輸出（藍圖 §3.6），SQL 要能當欄位被 grep、
// 被聚合、被接上 trace ID。`db.Debug()` 印出來的東西這三件都做不到。
//
// 為什麼不用 `gormlogger.New(writer, config)` 的內建實作：它只吃一個
// `Printf(string, ...any)` 介面，所有欄位會被壓成一句字串——那等於把結構化
// log 退化回文字檔，只是換了個出口。
type gormSlog struct {
	logger        *slog.Logger
	level         gormlogger.LogLevel
	slowThreshold time.Duration
}

// NewGormLogger 組出一個把 SQL 寫進 slog 的 GORM logger。
//
// sqlLog 為 false 時只留警告與錯誤（等同 GORM 的 Warn 等級），
// 讓「壓測時關掉 SQL 輸出」不必換成另一個型別。
//
// slowThreshold 是慢查詢的門檻，超過就升級成 Warn。200ms 對帳務熱路徑而言
// 已經是「該去看一眼」的等級——三條語句都是主鍵或唯一索引點查。
func NewGormLogger(logger *slog.Logger, sqlLog bool) gormlogger.Interface {
	if logger == nil {
		logger = slog.Default()
	}
	level := gormlogger.Warn
	if sqlLog {
		level = gormlogger.Info
	}
	return gormSlog{
		logger:        logger,
		level:         level,
		slowThreshold: 200 * time.Millisecond,
	}
}

// LogMode 回傳一個換了等級的**副本**。
//
// ⚠️ 接收者是值不是指標，這是刻意的：GORM 會在 `db.Session(...)` 之類的地方
// 呼叫它，期待拿到新的 logger 而**原本那個不受影響**。用指標接收者就地改欄位
// 的話，某一次 `db.Debug()` 會把整個連線池的 log 等級一起改掉——而且是無聲的。
func (g gormSlog) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	g.level = level
	return g
}

func (g gormSlog) Info(ctx context.Context, msg string, args ...any) {
	if g.level >= gormlogger.Info {
		g.logger.InfoContext(ctx, fmt.Sprintf(msg, args...))
	}
}

func (g gormSlog) Warn(ctx context.Context, msg string, args ...any) {
	if g.level >= gormlogger.Warn {
		g.logger.WarnContext(ctx, fmt.Sprintf(msg, args...))
	}
}

func (g gormSlog) Error(ctx context.Context, msg string, args ...any) {
	if g.level >= gormlogger.Error {
		g.logger.ErrorContext(ctx, fmt.Sprintf(msg, args...))
	}
}

// Trace 是每一條 SQL 都會經過的地方。
func (g gormSlog) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if g.level <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	sql, rows := fc()
	attrs := []any{
		"sql", sql,
		// ⭐ rows 是這裡最重要的欄位。帳務的樂觀鎖與條件扣款靠的就是
		// 「受影響列數是不是 1」（地雷 #3），出事後回頭查 log 時，
		// 一筆 rows=0 的 UPDATE 就是全部的答案。
		"rows", rows,
		"elapsedMs", elapsed.Milliseconds(),
	}

	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && g.level >= gormlogger.Error:
		// ⚠️ ErrRecordNotFound 要排除。findTxByKey 的「查不到」是冪等快路徑的
		// **正常結果**，把它記成 ERROR 會讓每一筆正常入帳都噴一則假警報，
		// 然後真正的錯誤就淹沒在裡面了。
		g.logger.ErrorContext(ctx, "SQL 執行失敗", append(attrs, "err", err)...)
	case g.slowThreshold > 0 && elapsed > g.slowThreshold && g.level >= gormlogger.Warn:
		g.logger.WarnContext(ctx, "SQL 慢查詢",
			append(attrs, "slowThresholdMs", g.slowThreshold.Milliseconds())...)
	case g.level >= gormlogger.Info:
		g.logger.InfoContext(ctx, "SQL", attrs...)
	}
}
