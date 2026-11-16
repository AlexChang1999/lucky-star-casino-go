package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/domain"
	"github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/quota"
	walletstore "github.com/AlexChang1999/lucky-star-casino-go/internal/wallet/store"
)

// 贈禮：兩步冪等 + 補償（docs/ADR-005）。
//
// 送出方 debit 與接收方 credit 是兩筆各自獨立的交易，中間隔著一張狀態單
// （gift_transfers）。狀態單**先寫、錢後動**，所以行程死在任何一步，重啟後看狀態單
// 就知道下一步：
//
//	PENDING ──debit──▶ DEBITED ──credit──▶ COMPLETED
//	   │                  │
//	   │ 扣款被拒            │ 入帳失敗滿 GiftMaxCreditAttempts 次，或接收方錢包不存在
//	   ▼                  ▼
//	（刪單）            REFUNDING ──refund──▶ REFUNDED
//
// ⭐ 三步的冪等鍵都由呼叫端那把鍵衍生、而且**永遠不變**（地雷 #4）。同一步被請求與
// 背景重試各做一次、或被重試十次，帳上都只有一筆。

const (
	// GiftMaxCreditAttempts：入帳最多嘗試幾次（含請求當下那一次），用盡就退款。
	GiftMaxCreditAttempts = 5

	// giftRetryBase：入帳失敗後的退避基數，每次加倍 → 1s、2s、4s、8s。
	giftRetryBase = time.Second
	// giftRetryCap：扣款確認與退款沒有次數上限（放棄就等於錢卡在半路），所以退避要有上限。
	giftRetryCap = 5 * time.Minute

	// giftInFlightGrace：剛立單、剛扣款的狀態單在這段時間內背景重試不碰。
	// 那一步正在請求裡進行中；背景搶著做雖然不會重複記帳，但會平白消耗一次入帳嘗試。
	giftInFlightGrace = 30 * time.Second

	// giftRetryBatch：背景重試一輪最多處理幾張。
	giftRetryBatch = 50
	// GiftRetryInterval：背景重試多久掃一次。退避最小單位是 1 秒，掃得更密沒有意義。
	GiftRetryInterval = time.Second

	// giftMaxSteps 是 advanceGift 迴圈的保險絲。正常路徑最多三步（扣款、入帳、退款），
	// 加上「被別人先轉走而重讀」的次數也遠小於它。
	giftMaxSteps = 8
)

// 這幾個 sentinel 只給 Error.Err 與日誌用；對外訊息由 handler 依 Code 決定。
var (
	errGiftToSelf       = errors.New("不可贈送給自己")
	errGiftRefunded     = errors.New("贈禮入帳失敗，已退回送出方")
	errGiftKeyCollision = errors.New("贈禮冪等鍵命中了不屬於這筆贈禮的流水")
	errGiftStuck        = errors.New("贈禮狀態單轉移次數異常")

	errGiftKeyOwnedByOther = errors.New("冪等鍵已被另一位送出方的贈禮使用")
)

// GiftResult 是一次贈禮目前的結果。Status 不是 COMPLETED 時，
// 還沒發生的那幾步的欄位是 nil（地雷 #33）。
type GiftResult struct {
	SenderID             int64
	ReceiverID           int64
	Amount               int64
	DebitTransactionID   *int64
	CreditTransactionID  *int64
	SenderBalanceAfter   *int64
	ReceiverBalanceAfter *int64
	// Idempotent 為 true 代表這次呼叫沒有立新單，回的是既有那張單的現況。
	Idempotent bool
	Status     walletstore.GiftStatus
}

// Gift 送出一筆贈禮。key 是呼叫端的冪等鍵：帶同一把鍵重送，得到的是同一筆贈禮的現況。
//
// 步驟順序對齊 Java `GiftService.gift` 的對外行為（同時成立時先回哪一個錯誤）：
// 送給自己 → 冪等命中 → 當日額度 → 送出方錢包 / 餘額 → 接收方錢包。
//
// 回傳的三種形狀：
//   - (結果, nil) 且 Status = COMPLETED：成功。
//   - (結果, nil) 且 Status 為其他：錢已離開送出方、尚未到帳，背景會繼續處理。
//   - (_, *Error)：沒有成立。CodeGiftRefunded 代表錢動過但已經退回。
func (s *Service) Gift(ctx context.Context, senderID, receiverID, amount int64, key string) (GiftResult, error) {
	if senderID == receiverID {
		return GiftResult{}, &Error{Code: CodeInvalidGift, Err: errGiftToSelf}
	}
	// 非正整數的玩家 ID 不可能有錢包。擋在這裡而不是讓它走到 domain 的驗證，
	// 是因為對外要回「錢包不存在」（Java 的行為），不是 500。
	if senderID <= 0 {
		return GiftResult{}, &Error{Code: CodeGiftSenderNotFound, Err: walletstore.ErrWalletNotFound}
	}
	if receiverID <= 0 {
		return GiftResult{}, &Error{Code: CodeGiftReceiverNotFound, Err: walletstore.ErrWalletNotFound}
	}

	// 冪等預檢排在額度之前：重送不可以再吃一次當日額度。
	existing, err := s.gifts.GiftByKey(ctx, key)
	switch {
	case err == nil:
		return s.replayGift(existing, senderID)
	case !errors.Is(err, walletstore.ErrGiftNotFound):
		s.logger.Error("查詢贈禮狀態單失敗", "senderID", senderID, "idempotencyKey", key, "err", err)
		return GiftResult{}, &Error{Code: CodeInternal, Err: err}
	}

	amt := domain.Amount(amount)
	if err := s.quota.ReserveGiftQuota(ctx, senderID, receiverID, amt); err != nil {
		switch {
		case errors.Is(err, quota.ErrGiftSentLimitExceeded):
			return GiftResult{}, &Error{Code: CodeGiftSentLimitExceeded, Err: err}
		case errors.Is(err, quota.ErrGiftRecvLimitExceeded):
			return GiftResult{}, &Error{Code: CodeGiftRecvLimitExceeded, Err: err}
		}
		// fail-closed：額度狀態讀不到時不放行（理由見 internal/wallet/quota 的套件註解）。
		s.logger.Error("預扣贈禮當日額度失敗（fail-closed，不送出）", "senderID", senderID, "receiverID", receiverID, "err", err)
		return GiftResult{}, &Error{Code: CodeInternal, Err: err}
	}
	// 從這裡開始，任何「什麼都沒動就結束」的出口都要回補額度。
	// WithoutCancel：客戶端斷線取消了 ctx，回補也不該跟著失敗（失敗只會讓額度偏嚴到午夜）。
	release := func() { s.quota.ReleaseGiftQuota(context.WithoutCancel(ctx), senderID, receiverID, amt) }

	if gerr := s.precheckGift(ctx, senderID, receiverID, amount); gerr != nil {
		release()
		return GiftResult{}, gerr
	}

	// ⭐ 先立單再動錢。順序反過來的話，扣款成功後行程死掉就沒有任何紀錄說這筆錢要去哪。
	g, err := s.gifts.InsertGift(ctx, key, senderID, receiverID, amt, s.now().Add(giftInFlightGrace))
	if err != nil {
		release()
		if errors.Is(err, walletstore.ErrGiftDuplicateKey) {
			// 同鍵的另一個請求剛好先立了單：這次什麼都沒做，回它的現況。
			winner, werr := s.gifts.GiftByKey(ctx, key)
			if werr != nil {
				s.logger.Error("贈禮冪等鍵衝突但讀不到贏家", "idempotencyKey", key, "err", werr)
				return GiftResult{}, &Error{Code: CodeInternal, Err: werr}
			}
			return s.replayGift(winner, senderID)
		}
		s.logger.Error("建立贈禮狀態單失敗", "senderID", senderID, "idempotencyKey", key, "err", err)
		return GiftResult{}, &Error{Code: CodeInternal, Err: err}
	}

	g, deleted, stepErr := s.advanceGift(ctx, g)
	if deleted {
		// 扣款被明確拒絕、狀態單已刪：什麼都沒動。
		release()
		return GiftResult{}, stepErr
	}
	switch g.Status {
	case walletstore.GiftPending:
		// 扣款結果不明（例如連線斷在 commit 上）。額度**不回補**：這筆贈禮可能已經扣款，
		// 背景會用同一把鍵確認並做完它。
		if stepErr == nil {
			// 走不到：停在 PENDING 一定有讓它停下來的錯誤。防的是回出「零值結果 + nil」。
			stepErr = &Error{Code: CodeInternal, Err: errGiftStuck}
		}
		return GiftResult{}, stepErr
	case walletstore.GiftRefunded:
		return GiftResult{}, &Error{Code: CodeGiftRefunded, Err: errGiftRefunded}
	default:
		// COMPLETED，或錢已離開送出方、入帳 / 退款還在路上（背景接手）。
		return giftResultOf(g, false), nil
	}
}

// precheckGift 在動錢之前擋掉「一定不會成功」的情況，讓常見的失敗不留下任何痕跡。
//
// ⚠️ 這只是預檢，不是保證：檢查完到真正扣款之間餘額可能變、錢包可能被停用。
// 真正的防線是 debit 的條件 UPDATE，以及入帳失敗後的退款。
// ⚠️ 餘額看的是**總餘額**，對齊 Java（`sender.getBalance() < amount`）。
func (s *Service) precheckGift(ctx context.Context, senderID, receiverID, amount int64) *Error {
	balance, err := s.store.Balance(ctx, senderID)
	switch {
	case errors.Is(err, walletstore.ErrWalletNotFound):
		return &Error{Code: CodeGiftSenderNotFound, Err: err}
	case err != nil:
		s.logger.Error("贈禮預檢讀取送出方餘額失敗", "senderID", senderID, "err", err)
		return &Error{Code: CodeInternal, Err: err}
	case balance < amount:
		return &Error{Code: CodeInsufficientBalance, Err: walletstore.ErrInsufficientBalance}
	}

	_, err = s.store.Balance(ctx, receiverID)
	switch {
	case errors.Is(err, walletstore.ErrWalletNotFound):
		return &Error{Code: CodeGiftReceiverNotFound, Err: err}
	case err != nil:
		s.logger.Error("贈禮預檢讀取接收方錢包失敗", "receiverID", receiverID, "err", err)
		return &Error{Code: CodeInternal, Err: err}
	}
	return nil
}

// replayGift 把一張既有的狀態單翻成冪等重送的回應。
//
// ⚠️ **刻意與 Java 分歧**：Java 的 `buildIdempotentResponse` 不看呼叫者是誰，
// 任何人帶同一把鍵都拿得到原贈禮的內容——包含原送出方的餘額。這裡只回給原送出方，
// 其他人一律當成鍵衝突（記在 docs/ADR-005）。
// 同一個送出方用同一把鍵改送別人或改金額，則照 Java 回原本那一筆。
func (s *Service) replayGift(g walletstore.GiftTransfer, senderID int64) (GiftResult, error) {
	if g.SenderID != senderID {
		s.logger.Warn("贈禮冪等鍵屬於另一位送出方，拒絕重送",
			"idempotencyKey", g.IdempotencyKey, "ownerID", g.SenderID, "callerID", senderID)
		return GiftResult{}, &Error{Code: CodeGiftKeyConflict, Err: errGiftKeyOwnedByOther}
	}
	if g.Status == walletstore.GiftRefunded {
		return GiftResult{}, &Error{Code: CodeGiftRefunded, Err: errGiftRefunded}
	}
	return giftResultOf(g, true), nil
}

func giftResultOf(g walletstore.GiftTransfer, idempotent bool) GiftResult {
	return GiftResult{
		SenderID:             g.SenderID,
		ReceiverID:           g.ReceiverID,
		Amount:               g.Amount,
		DebitTransactionID:   g.DebitTransactionID,
		CreditTransactionID:  g.CreditTransactionID,
		SenderBalanceAfter:   g.SenderBalanceAfter,
		ReceiverBalanceAfter: g.ReceiverBalanceAfter,
		Idempotent:           idempotent,
		Status:               g.Status,
	}
}

// giftMovements 由狀態單組出三步的帳務意圖。
//
// ⭐ 這是冪等鍵**唯一**的產生處，而且只吃狀態單上的欄位：請求與背景重試拿到的
// 必定是同一把鍵（地雷 #4）。鍵的形狀與 referenceId 對齊 Java `GiftService` /
// `GiftTransferService`；`:gift:refund` 是本專案新增的第三把。
func giftMovements(g walletstore.GiftTransfer) (debit, credit, refund domain.Movement, err error) {
	amt := domain.Amount(g.Amount)
	debit, err = domain.NewGiftDebit(g.SenderID, amt, g.IdempotencyKey+":gift:debit", fmt.Sprintf("gift-to:%d", g.ReceiverID))
	if err != nil {
		return
	}
	credit, err = domain.NewCredit(g.ReceiverID, amt, domain.SubTypeGift, g.IdempotencyKey+":gift:credit", fmt.Sprintf("gift-from:%d", g.SenderID), 0)
	if err != nil {
		return
	}
	refund, err = domain.NewCredit(g.SenderID, amt, domain.SubTypeRefund, g.IdempotencyKey+":gift:refund", fmt.Sprintf("gift-refund:%d", g.ReceiverID), 0)
	return
}

// advanceGift 把一張狀態單往前推，直到終態或某一步失敗為止。請求與背景重試共用這一條路。
//
// 回傳：
//   - g：推進後的狀態單。
//   - deleted：true 代表扣款被明確拒絕、狀態單已刪（什麼都沒動）。
//   - err：讓它停下來的那一步的錯誤（已在起點記過日誌）。走到終態時為 nil。
func (s *Service) advanceGift(ctx context.Context, g walletstore.GiftTransfer) (_ walletstore.GiftTransfer, deleted bool, _ error) {
	debit, credit, refund, err := giftMovements(g)
	if err != nil {
		s.logger.Error("贈禮狀態單組不出合法的帳務意圖（資料錯誤）", "giftID", g.ID, "err", err)
		return g, false, &Error{Code: CodeInternal, Err: err}
	}

	for range giftMaxSteps {
		var (
			moved   bool  // 這一步是否把狀態單轉到了下一個狀態
			stepErr error // 這一步的錯誤；非 nil 就停
		)
		switch g.Status {
		case walletstore.GiftPending:
			g, moved, deleted, stepErr = s.giftDebitStep(ctx, g, debit)
		case walletstore.GiftDebited:
			g, moved, stepErr = s.giftCreditStep(ctx, g, credit)
		case walletstore.GiftRefunding:
			g, moved, stepErr = s.giftRefundStep(ctx, g, refund)
		default:
			return g, false, nil // COMPLETED / REFUNDED
		}
		if deleted || stepErr != nil {
			return g, deleted, stepErr
		}
		if moved {
			continue
		}
		// 條件式轉移沒改到任何一列：別人（另一個請求或另一個副本）已經先轉走了。
		// 重讀之後照它現在的狀態繼續。
		fresh, rerr := s.gifts.GiftByID(ctx, g.ID)
		if errors.Is(rerr, walletstore.ErrGiftNotFound) {
			return g, true, &Error{Code: CodeInternal, Err: rerr}
		}
		if rerr != nil {
			s.logger.Error("重讀贈禮狀態單失敗", "giftID", g.ID, "err", rerr)
			return g, false, &Error{Code: CodeInternal, Err: rerr}
		}
		g = fresh
	}
	s.logger.Error("贈禮狀態單轉移次數異常，停止推進", "giftID", g.ID, "status", g.Status)
	return g, false, &Error{Code: CodeInternal, Err: errGiftStuck}
}

// giftDebitStep：PENDING → DEBITED。
func (s *Service) giftDebitStep(ctx context.Context, g walletstore.GiftTransfer, debit domain.Movement) (_ walletstore.GiftTransfer, moved, deleted bool, _ error) {
	r, err := s.Debit(ctx, debit)
	if err == nil && giftKeyCollision(r.Idempotent, r.PlayerID, r.Amount, debit) {
		err = s.giftCollision(debit)
	}
	if err != nil {
		switch CodeOf(err) {
		case CodeInsufficientBalance, CodeWalletNotFound:
			// 明確被拒：交易已回滾，什麼都沒動。刪單釋放冪等鍵，玩家補足餘額後可以再送一次。
			ok, derr := s.gifts.DeletePendingGift(ctx, g.ID)
			if derr != nil {
				// 刪不掉就留著：下一輪重試會再被拒一次、再刪一次。
				s.logger.Error("刪除被拒的贈禮狀態單失敗", "giftID", g.ID, "err", derr)
				return g, false, false, err
			}
			if !ok {
				return g, false, false, nil // 已被別人轉走，交給外層重讀
			}
			return g, false, true, err
		}
		// 結果不明（連線斷在 commit、DB 不可用…）：不能刪也不能往下走，排重試用同一把鍵確認。
		s.scheduleGiftRetry(ctx, &g, err)
		return g, false, false, err
	}

	ok, merr := s.gifts.MarkGiftDebited(ctx, g.ID, r.TransactionID, r.BalanceAfter, s.now().Add(giftInFlightGrace))
	if merr != nil {
		// 錢已經扣了但狀態單沒更新。它還是 PENDING，背景會再跑一次這一步：
		// debit 冪等命中 → 再標一次。
		s.logger.Error("贈禮已扣款但狀態單更新失敗，待背景重試", "giftID", g.ID, "err", merr)
		return g, false, false, &Error{Code: CodeInternal, Err: merr}
	}
	if !ok {
		return g, false, false, nil
	}
	g.Status = walletstore.GiftDebited
	g.Attempts = 0
	g.DebitTransactionID = &r.TransactionID
	g.SenderBalanceAfter = &r.BalanceAfter
	return g, true, false, nil
}

// giftCreditStep：DEBITED → COMPLETED，失敗滿次數（或接收方錢包不存在）→ REFUNDING。
func (s *Service) giftCreditStep(ctx context.Context, g walletstore.GiftTransfer, credit domain.Movement) (_ walletstore.GiftTransfer, moved bool, _ error) {
	r, err := s.Credit(ctx, credit)
	if err == nil && giftKeyCollision(r.Idempotent, r.PlayerID, r.Amount, credit) {
		err = s.giftCollision(credit)
	}
	if err != nil {
		attempts := g.Attempts + 1
		// 接收方錢包不存在是不會自己好的錯誤，重試只是讓送出方的錢多卡幾秒。
		//
		// ⚠️ 次數用盡只對「確定沒入帳」的錯誤才算數（樂觀鎖衝突）。CodeInternal 是結果不明
		// （commit 逾時、連線斷、關機取消）：入帳可能已經提交，這時退款就是兩邊都拿到錢。
		// 結果不明就繼續用同一把鍵重試——冪等命中會把狀態單完成，代價只是錢多卡一會兒。
		giveUp := CodeOf(err) == CodeWalletNotFound ||
			(attempts >= GiftMaxCreditAttempts && CodeOf(err) != CodeInternal)
		if giveUp {
			// 退款前最後一道防線：入帳流水若已存在（別的副本剛做完、或上一次其實成功了），
			// 就不退款，排重試讓冪等命中來完成。查不到結果也當成「不確定」，不退款。
			landed, lerr := s.gifts.CreditLanded(ctx, credit.IdempotencyKey)
			if lerr != nil || landed {
				giveUp = false
			}
		}
		if !giveUp {
			next := s.now().Add(giftBackoff(attempts))
			if _, serr := s.gifts.ScheduleGiftRetry(ctx, g.ID, g.Status, attempts, next, err.Error()); serr != nil {
				// 排不進去就維持原本的 next_attempt_at，背景照樣會撿到；只是這次失敗沒被計數。
				s.logger.Error("記錄贈禮入帳失敗次數失敗", "giftID", g.ID, "err", serr)
			}
			g.Attempts = attempts
			return g, false, err
		}

		ok, merr := s.gifts.MarkGiftRefunding(ctx, g.ID, s.now(), err.Error())
		if merr != nil {
			s.logger.Error("贈禮轉入退款狀態失敗，待背景重試", "giftID", g.ID, "err", merr)
			return g, false, &Error{Code: CodeInternal, Err: merr}
		}
		if !ok {
			return g, false, nil
		}
		s.logger.Warn("贈禮入帳放棄，改為退款給送出方",
			"giftID", g.ID, "senderID", g.SenderID, "receiverID", g.ReceiverID,
			"amount", g.Amount, "attempts", attempts, "err", err)
		g.Status = walletstore.GiftRefunding
		g.Attempts = 0
		return g, true, nil
	}

	ok, merr := s.gifts.MarkGiftCompleted(ctx, g.ID, r.TransactionID, r.BalanceAfter)
	if merr != nil {
		// 已入帳但狀態單沒更新：它還是 DEBITED，背景再入帳一次會冪等命中，然後再標一次。
		s.logger.Error("贈禮已入帳但狀態單更新失敗，待背景重試", "giftID", g.ID, "err", merr)
		return g, false, &Error{Code: CodeInternal, Err: merr}
	}
	if !ok {
		return g, false, nil
	}
	g.Status = walletstore.GiftCompleted
	g.CreditTransactionID = &r.TransactionID
	g.ReceiverBalanceAfter = &r.BalanceAfter
	return g, true, nil
}

// giftRefundStep：REFUNDING → REFUNDED。沒有次數上限——放棄退款等於送出方的錢憑空消失。
//
// ⚠️ 退款成功後**不回補當日額度**：退款可能發生在隔天，而 ReleaseGiftQuota 用的是
// 「現在」的日期，回補會扣到隔天的計數上。少回補只會讓當天的額度偏嚴、午夜自動歸零
// （與 Java 對「多計」的取捨相同）。
func (s *Service) giftRefundStep(ctx context.Context, g walletstore.GiftTransfer, refund domain.Movement) (_ walletstore.GiftTransfer, moved bool, _ error) {
	r, err := s.Credit(ctx, refund)
	if err == nil && giftKeyCollision(r.Idempotent, r.PlayerID, r.Amount, refund) {
		err = s.giftCollision(refund)
	}
	if err != nil {
		s.scheduleGiftRetry(ctx, &g, err)
		return g, false, err
	}

	ok, merr := s.gifts.MarkGiftRefunded(ctx, g.ID, r.BalanceAfter)
	if merr != nil {
		s.logger.Error("贈禮已退款但狀態單更新失敗，待背景重試", "giftID", g.ID, "err", merr)
		return g, false, &Error{Code: CodeInternal, Err: merr}
	}
	if !ok {
		return g, false, nil
	}
	s.logger.Info("贈禮已退回送出方", "giftID", g.ID, "senderID", g.SenderID, "amount", g.Amount)
	g.Status = walletstore.GiftRefunded
	g.SenderBalanceAfter = &r.BalanceAfter
	return g, true, nil
}

// scheduleGiftRetry 給「沒有次數上限」的兩步（確認扣款、退款）記一次失敗並排下一次。
func (s *Service) scheduleGiftRetry(ctx context.Context, g *walletstore.GiftTransfer, cause error) {
	attempts := g.Attempts + 1
	if _, err := s.gifts.ScheduleGiftRetry(ctx, g.ID, g.Status, attempts, s.now().Add(giftBackoff(attempts)), cause.Error()); err != nil {
		s.logger.Error("排定贈禮重試失敗", "giftID", g.ID, "status", g.Status, "err", err)
		return
	}
	g.Attempts = attempts
}

// giftKeyCollision 回答「這次冪等命中的流水，是不是其實不屬於這筆贈禮」。
//
// 冪等命中時 store 回的是**原交易**的玩家與金額。贈禮的鍵由狀態單衍生，正常情況下
// 原交易就是自己之前寫的那一筆；但若那把鍵先被別的流水占用（鍵的命名空間撞了），
// 把它當成功會讓狀態單走到 COMPLETED 而錢根本沒進到接收方。
func giftKeyCollision(idempotent bool, playerID, amount int64, want domain.Movement) bool {
	return idempotent && (playerID != want.PlayerID || amount != int64(want.Amount))
}

func (s *Service) giftCollision(m domain.Movement) *Error {
	s.logger.Error("贈禮的冪等鍵命中了別的流水，拒絕把它當成這一步已完成",
		"type", m.Type, "playerID", m.PlayerID, "amount", int64(m.Amount), "idempotencyKey", m.IdempotencyKey)
	return &Error{Code: CodeInternal, Err: errGiftKeyCollision}
}

// giftBackoff 回傳第 attempts 次失敗之後要等多久：1s、2s、4s、8s…，上限 giftRetryCap。
func giftBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	// 位移量先夾住：attempts 沒有上限（退款會一直重試），直接位移會溢位成負數。
	shift := min(attempts-1, 20)
	return min(giftRetryBase<<shift, giftRetryCap)
}

// RetryDueGifts 把到期的未完成狀態單各往前推一次，回傳處理了幾張。
// 每一張的失敗都已在起點記過日誌，這裡不重複記。
func (s *Service) RetryDueGifts(ctx context.Context) (int, error) {
	due, err := s.gifts.DueGifts(ctx, s.now(), giftRetryBatch)
	if err != nil {
		return 0, err
	}
	for _, g := range due {
		if ctx.Err() != nil {
			break
		}
		_, _, _ = s.advanceGift(ctx, g)
	}
	return len(due), nil
}

// RunGiftRetries 是背景重試的迴圈，跑到 ctx 被取消為止。由 main 起一個 goroutine 呼叫。
//
// ⚠️ 關機時進行中的那一步會因 ctx 取消而失敗，狀態單維持原狀，下次啟動接著做——
// 每一步都冪等，中斷在任何位置都安全。
func (s *Service) RunGiftRetries(ctx context.Context) {
	s.logger.Info("贈禮背景重試啟動", "interval", GiftRetryInterval.String(), "maxCreditAttempts", GiftMaxCreditAttempts)
	ticker := time.NewTicker(GiftRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("贈禮背景重試停止")
			return
		case <-ticker.C:
		}
		if _, err := s.RetryDueGifts(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("撈取待重試的贈禮失敗，下一輪再試", "err", err)
		}
	}
}
