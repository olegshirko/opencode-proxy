# Qoder Upstream Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Второй upstream — подписка Qoder — за тем же прокси: PAT меняется на jobToken внутри прокси, трафик идёт в `https://api1.qoder.sh/model/v1/chat/completions` (OpenAI-совместимый), клиент видит Anthropic Messages как сейчас.

**Architecture:** Два новых пакета: `internal/qoderauth` (token manager: exchange → кэш → refresh → invalidate) и `internal/qoderchat` (транслятор Anthropic ⇄ Qoder/OpenAI, включая SSE). Прокси ветвит запрос по `Route.Auth == "qoder"`: для стрима upstream-ответ оборачивается в транслирующий reader, поэтому существующий конвейер `serveStream` (keepalive, thinking-plates, учёт usage) работает без изменений. Шедулер: ячейки с префиксом провайдера, prefill-обучение только от OpenCode.

**Tech Stack:** Go 1.27, только stdlib (net/http, encoding/json, testing). Тесты — httptest.

**Spec:** `docs/superpowers/specs/2026-11-10-qoder-upstream-design.md` (читать вместе с планом; там схема авторизации, ендпоинты, заголовки и формат запроса с оффсетами кода воркера).

## Global Constraints

- Только stdlib; новых зависимостей в `go.mod` не добавлять.
- PAT/токены никогда не попадают в логи, capture и ответы admin: в reqlog уже маскируются `Authorization`/`x-api-key` (reqlog/log.go:301); новые поля (`Status`, конфиг) не содержат самих секретов — только пути и счётчики.
- Без секции `qoder` в конфиге и без `auth:"qoder"` в роутах поведение прокси байт-в-байт прежнее.
- Пикер-имена моделей Qoder — `claude-qoder-<short>`; дубликат пикер-имени = ошибка загрузки конфига, не перекрытие.
- `models.default`/`models.background` не указывают на модели Qoder; фоновый трафик остаётся на OpenCode.
- Контракты, помеченные в спеке «проверить живым PAT», фиксируются Task 1 в `var/qoder-research/fixtures/contract.json`; последующие задачи берут значения оттуда (значения по умолчанию — из спеки, спайк их подтверждает или правит).
- Коммиты: обычные сообщения без AI-атрибуции (правило пользователя).

## Review Focus

1. **Секреты**: jobToken/refresh нигде не сериализуются в `/admin/status`, reqlog, capture — ожидание: администратор видит только state/счётчики/время. Тест: `TestStatusNeverContainsToken` (Task 2).
2. **count_tokens всегда отвечает**: Claude Code зовёт его постоянно; на Qoder-роуте — локальная оценка, ни при каком валидном теле не ошибка. Тест: `TestEstimateTokensTypicalBody` (Task 3).
3. **Нет молчаливого фолбэка**: отсутствующий/просроченный PAT на Qoder-роуте даёт 502 с текстом про конфиг Qoder, а не уход в OpenCode. Тест: `TestQoderRouteMissingToken` (Task 6).
4. **Неизвестная фича — явная ошибка**: неизвестный `tool_choice`/ломаное тело → 400 с описанием, не искажённый upstream-запрос. Тест: `TestTranslateRequestUnsupportedToolChoice` (Task 3).
5. **Изоляция потолка префилла**: 429 от Qoder не снижает общий prefill-потолок OpenCode. Тест: `TestPrefillLearningOnlyFromOpenCode` (Task 8).

---

### Task 1: Contract spike (нужен PAT; блокирует значения, но не код Tasks 2–5)

**Files:**
- Create: `var/qoder-research/spike/main.go`
- Create: `var/qoder-research/fixtures/contract.json` (результат)

**Interfaces:**
- Produces: `fixtures/contract.json` со схемой:
  `{"bearer_kind": "job"|"access", "exchange": {"status": 200, "token_field": "token|device_token|access_token", "expires_field": "expires_at|expire_time", "expires_unit": "ms"|"s"}, "models": [{"id": "...", "context": 0, "pricing_input": 0, "pricing_output": 0}], "sample_chunks": ["..."], "finish_required": true|false}`

- [ ] **Step 1: Написать spike-скрипт** (не в `cmd/` — это одноразовый инструмент; читает PAT из `var/qoder-research/pat.txt`, флаги `-model`, `-prompt`): POST `{openapi}/api/v1/jobToken/exchange` с `{"personal_token": <PAT>}` → дамп полей ответа (без значений токенов — только имена полей и типы сроков); `GET {api1}/api/v2/model/list` с Bearer → каталог; один streaming-запрос `POST {api1}/model/v1/chat/completions` (тело из спеки, секция «Инференс», `max_tokens: 16`) → дамп сырых SSE-строк; повтор с Bearer=jobToken и с Bearer=PAT, чтобы вычислить `bearer_kind`.
- [ ] **Step 2: Прогнать с PAT** (`go run ./var/qoder-research/spike` — временно, путь вне модуля: `go run` из каталога спайка с отдельным go.mod). Без PAT шаг блокирован — пропустить с пометкой в чекбоксе.
- [ ] **Step 3: Записать contract.json** и дописать секцию «проверить живым PAT» в спеке результатами.
- [ ] **Step 4: Commit** `var/qoder-research/fixtures/` (каталог спайка — нет: там нет секретов, но scripts spike с путём до pat.txt в git не идут; fixtures — идут).

### Task 2: `internal/qoderauth` — token manager

**Files:**
- Create: `internal/qoderauth/qoderauth.go`
- Test: `internal/qoderauth/qoderauth_test.go`

**Interfaces:**
- Consumes: `fixtures/contract.json` (порядок полей токена, unit сроков).
- Produces:
```go
type Config struct {
    OpenAPIBaseURL  string `json:"openapi_base_url"`  // default "https://openapi.qoder.sh"
    TokenEnv        string `json:"token_env"`         // приоритет 1
    TokenFile       string `json:"token_file"`        // приоритет 2 (~/ разворачивается)
    RefreshMarginMS int    `json:"refresh_margin_ms"` // default 60000
}
type Status struct {
    State      string    `json:"state"` // "empty"|"ready"|"error"
    HasRefresh bool      `json:"has_refresh"`
    ExpiresAt  time.Time `json:"expires_at,omitempty"`
    LastError  string    `json:"last_error,omitempty"`
    Exchanges  int64     `json:"exchanges"`
    Refreshes  int64     `json:"refreshes"`
    ReAuths    int64     `json:"reauths"`
}
func New(cfg Config, client *http.Client, now func() time.Time) *Manager
func (m *Manager) Token(ctx context.Context) (string, error) // exchange при пустом, refresh за margin до истечения
func (m *Manager) Invalidate()                                // 401 → полный re-exchange
func (m *Manager) Status() Status
```

- [ ] **Step 1: Тесты** (httptest-сервер как openapi):
  - `TestExchangeThenCached` — второй `Token()` не даёт второго POST.
  - `TestRefreshBeforeExpiry` — fake now двигают к `expiresAt - margin` → POST refresh, не exchange.
  - `TestInvalidateForcesReExchange`.
  - `TestMissingPATFails` — без env/файла → ошибка с текстом про `qoder.token_file/token_env`.
  - `TestStatusNeverContainsToken` — `Status()` marshalled JSON не содержит значений токенов.
  - Поле токена берётся в порядке `token` → `device_token` → `access_token` (порядок из воркера, `db(n,[...])`).
- [ ] **Step 2: Прогнать, убедиться в FAIL** (`go test ./internal/qoderauth/`).
- [ ] **Step 3: Реализация.** Мьютекс вокруг {token, refresh, expiresAt}; exchange: `POST {base}/api/v1/jobToken/exchange` тело `{"personal_token": PAT}`; refresh: `POST {base}/api/v1/jobToken/refresh` тело `{"refresh_token": ...}`; срок — по contract.json (`expires_at`, ms→`time.UnixMilli`); при 400/401/403 от refresh — Invalidate и один re-exchange.
- [ ] **Step 4: `go test ./internal/qoderauth/ -v`** → PASS.
- [ ] **Step 5: Commit** `feat: qoderauth token manager (PAT -> jobToken exchange/refresh)`.

### Task 3: `internal/qoderchat` — request translation + count_tokens

**Files:**
- Create: `internal/qoderchat/request.go`
- Test: `internal/qoderchat/request_test.go`
- Test fixture: `internal/qoderchat/testdata/anthropic_request.json`, `qoder_request.json`

**Interfaces:**
- Consumes: формат из спеки («Инференс», функция `QFn` воркера).
- Produces:
```go
type RequestMeta struct{ RequestID, SessionID, ClientType string }
func TranslateRequest(anthropic []byte, model string, meta RequestMeta) ([]byte, error)
func EstimateTokens(anthropic []byte) (int, error)
var ErrUnsupported = errors.New("qoderchat: unsupported request feature")
```
  Правила: `system` (строка и массив блоков) → system-message; `cache_control` срезается; thinking-блоки во входных сообщениях срезаются; `tools[].input_schema` → `parameters`; `tool_choice`: `auto`→`"auto"`, `{"type":"any"}`→`"required"`, `{"type":"tool","name":N}`→`{"type":"function","function":{"name":N}}`, остальное → `ErrUnsupported`; `max_tokens`→`max_tokens`, `temperature`, `stop_sequences`→`stop`; `metadata.user_id` игнорируется; в `metadata.context` идут request_id/session_id/client_type из meta; `stream:true` + `stream_options.include_usage:true` всегда (клиентский `stream` учитывает ветка прокси, не транслирующий: не-стрим собирается Task 4).

- [ ] **Step 1: Золотые тесты** — `testdata/anthropic_request.json` (system+multimodal+tools+tool_result+thinking-блок) → ожидаемый `qoder_request.json`; `TestTranslateRequestUnsupportedToolChoice` (`{"type":"hello"}` → `ErrUnsupported`); `TestEstimateTokensTypicalBody` — тело ~120k символов → оценка в [токены/4 ±25%], и оценка по `count_tokens`-семантике не зависит от `model`.
- [ ] **Step 2: FAIL** (`go test ./internal/qoderchat/`).
- [ ] **Step 3: Реализация.** Оценка токенов: сумма по сообщениям `ceil(len(content)/4)` + системные + per-tool 40; гарантированно не ошибка на валидном теле.
- [ ] **Step 4: PASS.**
- [ ] **Step 5: Commit** `feat: qoderchat request translation (anthropic -> openai-chat)`.

### Task 4: `internal/qoderchat` — response translation (SSE и non-stream)

**Files:**
- Create: `internal/qoderchat/response.go`
- Test: `internal/qoderchat/response_test.go`

**Interfaces:**
- Consumes: формат чанков — спека + `sample_chunks` из contract.json.
- Produces:
```go
type Usage struct{ InputTokens, OutputTokens, CacheReadTokens int }
// TranslateStream читает OpenAI SSE-чанки и пишет Anthropic SSE-события:
// message_start, content_block_{start,delta,stop}, message_delta, message_stop.
// reasoning_content -> thinking-блок. Возвращает usage финального чанка.
func TranslateStream(src io.Reader, w io.Writer, model string) (Usage, error)
// CollectNonStream: то же в один Anthropic Messages JSON (stream:false клиенты).
func CollectNonStream(src io.Reader, model string) ([]byte, Usage, error)
```
  Правила: `delta.content` → `text_delta`; `delta.reasoning_content` → `thinking_delta` (блок открывается/закрывается автоматически при чередовании с text); `finish_reason:"tool_calls"` → `stop_reason:"tool_use"`, `"length"`→`"max_tokens"`, `"stop"`→`end_turn`; `delta.tool_calls[i].function.{name,arguments}` копятся в один `content_block_start(type:tool_use)` + `input_json_delta`; usage из чанка с `usage` (include_usage) попадает в `message_delta.usage` и возвращается.

- [ ] **Step 1: Золотые тесты** — вход: сырые SSE-строки (включая `[DONE]`, чанк с разрывом tool_calls на два delta, чередование reasoning/text) → ожидаемые Anthropic-события; `TestCollectNonStreamToolCall`; `TestTranslateStreamUsageTotals`.
- [ ] **Step 2: FAIL.**
- [ ] **Step 3: Реализация** (конечный автомат: текущий блок {index,type}, буфер tool-args).
- [ ] **Step 4: PASS.**
- [ ] **Step 5: Commit** `feat: qoderchat stream/non-stream translation (openai -> anthropic)`.

### Task 5: Конфиг и роуты (`auth: "qoder"`)

**Files:**
- Modify: `internal/models/models.go` (Route: поле `Auth string json:"auth"`; helper `func (r *Route) IsQoder() bool`)
- Modify: `internal/models/models.go` — валидация при загрузке Settings: дубликат `picker` (в моделях и роутах вместе) → error
- Modify: `internal/config/config.go` (Config: `Qoder qoderauth.Config json:"qoder"`)
- Test: `internal/models/models_test.go`, `internal/config/config_test.go` (дополнить)

**Interfaces:**
- Produces: `Route.IsQoder()`; при пустой секции `qoder` поведение прежнее; `Token()` роута сохраняет приоритет route-level `token_env`/`token_file`, иначе менеджер.

- [ ] **Step 1: Тесты** — `TestRouteIsQoder`; `TestDuplicatePickerRejected` (две записи с одним picker → error при загрузке); `TestConfigQoderDefaults` (пустая секция → `OpenAPIBaseURL="https://openapi.qoder.sh"`, `RefreshMarginMS=60000`; hot-reload сохраняет поведение — существующие тесты конфига зелёные).
- [ ] **Step 2: FAIL.**
- [ ] **Step 3: Реализация.**
- [ ] **Step 4: `go test ./internal/...` PASS** (регресс: ничего не сломано).
- [ ] **Step 5: Commit** `feat: qoder route auth kind and config section`.

### Task 6: Прокси — не-стрим ветка и count_tokens

**Files:**
- Modify: `internal/proxy/proxy.go` (в handleRequest после резолва роута: `if route != nil && route.IsQoder()`)
- Create: `internal/proxy/qoder.go` (handleQoderNonStream, handleQoderCountTokens, buildQoderUpstream)
- Test: `internal/proxy/qoder_test.go`

**Interfaces:**
- Consumes: `qoderauth.Manager`, `qoderchat.*` (Tasks 2–4), `Route.IsQoder()` (Task 5).
- Produces:
```go
func buildQoderUpstream(ctx context.Context, cfg *config.Config, route *models.Route,
    tok *qoderauth.Manager, body []byte, meta qoderchat.RequestMeta, model string,
    stream bool) (*http.Request, error)
// body — уже переведённое qoderchat.TranslateRequest тело (вызывающая ветка
// делает TranslateRequest до вызова). URL: {cfg.Qoder.InferenceBaseURL}/model/v1/chat/completions
// (default https://api1.qoder.sh). Headers: Authorization Bearer,
// Accept: text/event-stream, Content-Type: application/json, X-Request-ID, X-Session-ID (uuid по запросу)
```
  count_tokens на Qoder-роуте вообще не ходит в сеть: `EstimateTokens` → `{"input_tokens": N}` (Anthropic-формат). non-stream: TranslateRequest → upstream → `CollectNonStream`. Ошибка трансляции (`ErrUnsupported`) → 400 с текстом; ошибка токена → 502 с упоминанием конфига Qoder (Review Focus 3); 401 → `tok.Invalidate()` + ровно один повтор (внутри `doAttempts` через newReq, Task 7 задаёт общий механизм).

- [ ] **Step 1: Тесты** (httptest upstream, фиктивный qoderauth Manager на том же httptest): `TestQoderCountTokensLocal` (сеть не пинается — upstream-счётчик запросов 0); `TestQoderNonStreamTranslation`; `TestQoderRouteMissingToken` (502, текст содержит "qoder"); `TestQoderUnsupportedFeature400`.
- [ ] **Step 2: FAIL.**
- [ ] **Step 3: Реализация.** UUID: `crypto/rand` v4 (в кодовой базе уже есть генерация id — найти через grep `randomUUID`/`uuid` и переиспользовать).
- [ ] **Step 4: `go test ./internal/proxy/` PASS.**
- [ ] **Step 5: Commit** `feat: qoder non-stream and count_tokens in proxy`.

### Task 7: Прокси — стрим через транслирующий reader + re-auth

**Files:**
- Modify: `internal/proxy/qoder.go` (translateReader: io.Pipe + goroutine TranslateStream)
- Modify: `internal/proxy/proxy.go` (serveStream: если route.IsQoder() — base.Body уже обёрнут)
- Test: `internal/proxy/qoder_test.go` (дополнить)

**Interfaces:**
- Consumes: `qoderchat.TranslateStream`, `doAttempts` newReq-механика.
- Produces: `func translateReader(src io.ReadCloser, model string) io.ReadCloser` — upstream OpenAI SSE на входе, Anthropic SSE на выходе; keepalive/thinking-plates существующего serveStream работают без изменений (видят Anthropic-события); 401: newReq вызывает `tok.Invalidate()` перед пересборкой — максимум один re-auth на клиентский запрос.

- [ ] **Step 1: Тесты** — `TestQoderStreamEndToEnd`: httptest upstream отдаёт OpenAI-чанки (с reasoning и tool_call), клиентский reader получает корректную Anthropic-последовательность, `usage` доходит до quota-учёта; `TestQoderReAuthOn401` (первый ответ 401, второй 200 → у клиента 200, счётчик Exchanges+2).
- [ ] **Step 2: FAIL.**
- [ ] **Step 3: Реализация.**
- [ ] **Step 4: PASS + `go test ./...`** (полный регресс).
- [ ] **Step 5: Commit** `feat: qoder streaming via translating reader with re-auth`.

### Task 8: Шедулер — изоляция провайдера

**Files:**
- Modify: `internal/proxy/proxy.go:249` (после RouteFor: `if route != nil && route.IsQoder() { cell = "qoder/" + cell }`)
- Modify: `internal/proxy/proxy.go:561` (guard: NotePrefillRefusal только для не-Qoder роутов — параметр `learnPrefill bool` у doAttempts/serveStream)
- Test: `internal/scheduler/scheduler_test.go`, `internal/proxy/qoder_test.go`

**Interfaces:**
- Produces: правило «prefill-обучение только от OpenCode» (Review Focus 5).

- [ ] **Step 1: Тест** — `TestPrefillLearningOnlyFromOpenCode`: два 429 (qoder-роут и обычный) → `PrefillRefusals` растёт на 1; ячейки qoder/opencode не конфликтуют (`TestQoderCellPrefix`).
- [ ] **Step 2: FAIL.**
- [ ] **Step 3: Реализация.**
- [ ] **Step 4: PASS.**
- [ ] **Step 5: Commit** `feat: per-provider scheduler cells, prefill learning gated to opencode`.

### Task 9: Каталог, admin, README

**Files:**
- Modify: `models.json` (записи моделей Qoder из `contract.json.models`, id = native id каталога, picker `claude-qoder-<short>`, behaves_as `claude-sonnet-5-5`, context/pricing из каталога)
- Modify: `config.json` — закомментированный-в-README пример секции `qoder` + `models.routes` пример (в README, не в рабочем конфиге)
- Modify: `internal/proxy/admin.go` (в `/admin/status`: блок `qoder`: `qoderauth.Status`)
- Modify: `README.md` (раздел «Qoder upstream»: PAT, включение, ограничение бюджета)
- Test: `internal/proxy/admin_test.go` (дополнить)

- [ ] **Step 1: Тест** — `TestAdminStatusIncludesQoder` (пустая секция → блок с `state:"empty"`, без секретов).
- [ ] **Step 2: FAIL → реализация → PASS.**
- [ ] **Step 3: models.json + README** (по фактическому каталогу из Task 1; без PAT — пункты помечены и Task откладывается, остальные сданы).
- [ ] **Step 4: `go test ./...` + `make verify`** (смоук на живом прокси).
- [ ] **Step 5: Commit** `feat: qoder catalog entries, admin token status, README`.

---

## Порядок и блокировки

Tasks 2–5, 8 не зависят от PAT и идут сразу. Task 1 (спайк) блокирует только *значения* для Task 9 (каталог) и подтверждает допущения Tasks 2/4; если спайк правит контракт (например Bearer=PAT) — правки локализованы в qoderauth/qoderchat fixtures. Task 6 после 2–5; Task 7 после 6; Task 9 последний.
