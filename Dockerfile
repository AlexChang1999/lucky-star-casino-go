# 本專案**所有**服務共用這一份 Dockerfile，用 `--build-arg SERVICE=<name>` 選一個。
#
#   docker build --build-arg SERVICE=wallet -t casino-go/wallet .
#
# ⚠️ 為什麼是一份而不是每個服務一份：七個服務的建置步驟完全相同（go build 一個
# static binary 塞進 scratch），複製七份的唯一結果是「其中一份忘了跟上」——
# 而那個漂移的症狀是「某個服務的映像莫名其妙大了 40 MB」或「只有它沒設 nonroot」。
# 差異化的東西（埠、環境變數、記憶體上限）本來就屬於 compose / K8s manifest，不屬於映像。

# ── 建置階段 ──────────────────────────────────────────────────────────────
# ⚠️ 版本要**釘死到 patch**。浮動的 `golang:1.26` 有天會滾到一個新版本，
# 於是「同一個 commit 建出不同的 binary」，而 CI 是綠的——這種不可重現性
# 要到出事時才會有人想起來查建置環境。
FROM golang:1.26.5-alpine AS build

# ca-certificates 不是給建置用的，是要**複製進最終映像**的（見下方執行階段）。
#
# ⚠️ 這裡刻意**不裝 git**：`.dockerignore` 把 `.git` 擋在建置上下文外（那是幾十 MB
# 且每次建置都會整包送進 daemon），所以 Go 根本偵測不到 VCS、也就不會去嵌
# vcs.revision。想知道「這顆映像是哪個 commit」請看 image label（見最下方的
# org.opencontainers.image.revision，CI 用 github.sha 帶進來）。
# ⚠️ 反過來說：哪天把 `.git` 放回上下文，就**必須**同時裝 git，
# 否則 go build 會直接失敗（`error obtaining VCS status`）——它偵測得到 .git
# 卻執行不了 git，那不是警告是錯誤。
RUN apk add --no-cache ca-certificates

WORKDIR /src

# 先只複製 go.mod / go.sum 再 download：只要依賴沒變，改任何一行 Go 程式碼
# 都能重用這一層。反過來寫（先 COPY . .）會讓每次改程式碼都重抓一次依賴。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG SERVICE=wallet
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# ⭐ CGO_ENABLED=0 是 scratch 映像的**前提**，不是效能選項。
# 開著 cgo 的話 binary 會動態連結 libc，塞進 scratch（裡面什麼都沒有）
# 就是啟動即死：`no such file or directory`——而那個訊息指的是**找不到動態
# 連結器**，不是找不到你的執行檔。這是 Go 容器化最容易被誤讀的錯誤訊息。
# ⚠️ 代價要知道：CGO_ENABLED=0 之下 net 套件走純 Go 的 DNS 解析，
# 不吃 /etc/nsswitch.conf。對容器環境（DNS 由編排器提供）沒有影響。
#
# -trimpath：把建置機的絕對路徑從 binary 裡拿掉（可重現性 + 不外洩本機路徑）
# -s -w：去掉符號表與 DWARF，省下約 30% 體積。⚠️ 代價是 panic 的 stack trace
#        仍有函式名（那來自 pclntab，不會被拿掉）但沒有行號資訊可供 delve 附加。
#        本專案的取捨是「正式映像要小、要除錯就本機重建」。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${SERVICE}

# ── 執行階段 ──────────────────────────────────────────────────────────────
# scratch＝完全空的映像：沒有 shell、沒有 libc、沒有套件管理器。
# 攻擊面小到沒有東西可以被利用，而且映像大小就是 binary 大小
# （notify-go 實測 18.6 MB，對照 Java 版的數百 MB——藍圖 §5 第 5 條那張圖）。
#
# ⚠️ 代價要寫清楚，不然下一個人會以為壞掉了：
#   - **不能 `docker exec` 進去看**（沒有 sh）。要除錯請用 `docker logs`、
#     或臨時改成 alpine 基底重建。
#   - **不能寫 HEALTHCHECK**（沒有 curl/wget）。健康檢查由 compose / K8s
#     從外面打 `GET /healthz`，那本來就比容器自己說自己健康可信。
#   - **沒有 tzdata**：本專案一律 UTC（地雷 #39），所以用不到。
#     哪天真的需要時區，正解是在該服務 `import _ "time/tzdata"` 把它編進
#     binary，而不是往映像裡塞檔案——前者跟著程式碼走，後者會在換基底映像時消失。
FROM scratch

# 憑證：目前 MySQL / Kafka 都是明文連線用不到，但區塊鏈模組（藍圖 §3.5）要打
# 第三方 RPC 的 HTTPS。少了它的症狀是 `x509: certificate signed by unknown
# authority`——看起來像憑證有問題，其實是這個映像裡一張根憑證都沒有。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/app /app

# ⚠️ 數字而不是名字：scratch 裡沒有 /etc/passwd，寫 `USER nonroot` 會在啟動時
# 找不到那個使用者。65532 是 distroless 的 nonroot 慣例值。
USER 65532:65532

# 這些是**文件**不是設定：EXPOSE 不會真的開埠，實際埠由 WALLET_HTTP_PORT 決定。
EXPOSE 8182

# ⚠️ GOMEMLIMIT 刻意**不寫在這裡**。它必須與容器的記憶體上限成對設定且略低於它
# （地雷 #24），而上限是部署時才知道的事——寫死在映像裡，換一個記憶體規格
# 就會變成「軟上限比硬上限還高」，等於沒設。它屬於 compose / K8s manifest。

ARG SERVICE=wallet
# VCS_REF 由 CI 帶入（github.sha）。⚠️ 它是「這顆映像對應哪個 commit」的**唯一**
# 答案，因為 .git 不在建置上下文裡（見建置階段的說明）。沒有它的話，
# 出事時只能靠時間戳去猜版本——而滾動更新期間新舊映像同時在跑。
ARG VCS_REF=unknown
LABEL org.opencontainers.image.source="https://github.com/AlexChang1999/lucky-star-casino-go" \
      org.opencontainers.image.title="lucky-star-casino-go/${SERVICE}" \
      org.opencontainers.image.description="幸運星幣城 Go 重構——${SERVICE} 服務" \
      org.opencontainers.image.revision="${VCS_REF}"

ENTRYPOINT ["/app"]
