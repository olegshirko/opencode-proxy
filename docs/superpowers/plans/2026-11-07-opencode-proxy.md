# opencode-proxy: рабочее состояние — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Довести `opencode-proxy` до рабочего сервиса: маппинг моделей OpenCode Go, устойчивость к 429, файловые логи с захватом трафика, управление по REST и автозапуск.

**Architecture:** Один Go-бинарник без внешних зависимостей. Конфиг читается из `config.json` по mtime, реестр моделей — из `models.json` (генерируется из каталога OpenCode). Прокси переписывает `model` в теле запроса, ретраит 429/5xx до отдачи заголовков, стримит SSE как есть. Админ-эндпоинты живут под `/admin/*` на том же порту и никогда не уходят наверх.

**Tech Stack:** Go 1.27, только стандартная библиотека (`net/http`, `encoding/json`, `log/slog` не используем — свой формат строки), launchd, Make.

**Spec:** `docs/2026-11-07-opencode-proxy-design.md`

## Global Constraints

- Go 1.27.1, darwin/arm64. Внешних модулей нет — только стандартная библиотека.
- Слушаем только `127.0.0.1:4000`. Апстрим: `https://opencode.ai/inference/go/anthropic`.
- Заголовок `x-opencode-session` обязателен в каждом запросе наверх.
- Ретраи: 5 попыток, первая 2000 мс, множитель 2, потолок 15000 мс на попытку, суммарный бюджет 60000 мс, джиттер ±20%.
- Ротация лога: 10 МБ × 5 файлов. `max_body_bytes` = 65536.
- Кэш `count_tokens`: 60 с, ключ — SHA-256 тела запроса.
- В upstream всегда уходит `model` в нижнем регистре.
- Секреты в лог не попадают никогда: `Authorization`, `x-api-key` маскируются как `***`, даже при включённом захвате заголовков.

## Review Focus

- **Клиент отвалился посреди SSE-стрима.** Ожидается: прокси прекращает чтение, закрывает апстрим-соединение, пишет строку со статусом и `aborted=true`, не течёт горутинами.
- **Тело запроса не JSON или без поля `model`.** Ожидается: проходит наверх без изменений, без 400 и без записи «подменено».
- **Апстрим вернул `Retry-After` больше бюджета.** Ожидается: ждём не дольше `max_wait_ms`, а не час.
- **Админка правит конфиг, пока запрос в полёте.** Ожидается: запрос доходит со снимком конфига, взятым на входе; паники и гонок нет.
- **Ошибка апстрима с гигантским телом.** Ожидается: в лог попадает не больше `max_body_bytes`, поле `truncated=true`, ответ клиенту уходит целиком.

---

### Task 1: Каркас проекта и загрузчик конфига

**Files:**
- Create: `go.mod`, `config.go`, `config_test.go`, `config.json`

**Interfaces:**
- Consumes: ничего.
- Produces:
  - `type Config struct { Listen, Upstream, SessionFallback string; Log LogConfig; Models ModelsConfig; Retry RetryConfig; Admin AdminConfig; ConnectTimeoutSeconds int }`
  - `type LogConfig struct { File, Level string; MaxMB, Keep int; Capture CaptureConfig; MaxBodyBytes int64 }`
  - `type CaptureConfig struct { RequestHeaders, RequestBody, ResponseHeaders, ResponseBody bool }`
  - `type ModelsConfig struct { Default, Background string; Aliases map[string]string }`
  - `type RetryConfig struct { MaxAttempts int; FirstWaitMS, MaxWaitMS, TotalBudgetMS int; Multiplier float64 }`
  - `type AdminConfig struct { Enabled bool; Token string }`
  - `func NewConfigLoader(path string) (*ConfigLoader, error)`
  - `func (l *ConfigLoader) Get() *Config` — снимок, безопасен для чтения из горутин
  - `func (l *ConfigLoader) ReloadIfChanged() error`
  - `func (l *ConfigLoader) Update(fn func(*Config) error) error` — меняет и пишет на диск
  - `func (l *ConfigLoader) Defaults() Config`

- [ ] **Step 1: Инициализировать репозиторий и модуль**

```bash
git init && go mod init opencode-proxy
```

- [ ] **Step 2: Написать падающий тест на значения по умолчанию**

```go
func TestDefaultsFillMissingFields(t *testing.T) {
    cfg := Defaults()
    if cfg.Listen != "127.0.0.1:4000" { t.Fatalf("listen = %q", cfg.Listen) }
    if cfg.Retry.MaxAttempts != 5 { t.Fatalf("attempts = %d", cfg.Retry.MaxAttempts) }
    if cfg.Log.MaxBodyBytes != 65536 { t.Fatalf("max_body = %d", cfg.Log.MaxBodyBytes) }
}
```

- [ ] **Step 3: Запустить и убедиться, что не компилируется**

Run: `go test ./... -run TestDefaultsFillMissingFields`
Expected: FAIL — `undefined: Defaults`

- [ ] **Step 4: Реализовать структуры и `Defaults()` в `config.go`**

`Defaults()` возвращает конфиг со всеми значениями из раздела Global Constraints. Пути к логу — через `os.UserHomeDir()`.

- [ ] **Step 5: Написать падающие тесты на загрузку, перезагрузку и сохранение**

```go
func TestNewConfigLoaderMergesDefaults(t *testing.T)      // файл с одним полем, остальное из Defaults()
func TestReloadIfChangedPicksUpEdit(t *testing.T)          // правка файла -> Get() видит новое значение
func TestUpdatePersistsToDisk(t *testing.T)                // Update(...) -> перечитать файл -> значение на месте
func TestInvalidConfigKeepsLastGood(t *testing.T)          // записать битый JSON -> ReloadIfChanged возвращает ошибку, Get() отдаёт прежний конфиг
```

`TestReloadIfChangedPicksUpEdit` перед второй проверкой делает `os.Chtimes` с явным сдвигом mtime на +2 с, чтобы не зависеть от разрешения ФС.

- [ ] **Step 6: Запустить тесты — должны упасть**

Run: `go test ./... -run 'TestNewConfigLoader|TestReload|TestUpdate|TestInvalid'`
Expected: FAIL — `undefined: NewConfigLoader`

- [ ] **Step 7: Реализовать `ConfigLoader`**

`sync.RWMutex` вокруг `*Config` и `time.Time` mtime. `Get()` возвращает копию структуры (алиасы копируются в новый map). `ReloadIfChanged` — `os.Stat`, сравнение mtime, декод через `json.Decoder` с `DisallowUnknownFields()` выключенным. `Update` — под замком, вызывает `fn`, затем `json.MarshalIndent` и атомарную запись через временный файл + `os.Rename`.

- [ ] **Step 8: Запустить тесты — должны пройти**

Run: `go test ./... -run 'TestNewConfigLoader|TestReload|TestUpdate|TestInvalid' -v`
Expected: PASS

- [ ] **Step 9: Создать `config.json` из `Defaults()`**

Файл содержит значения из спеки: `models.default` и `models.background` = `deepseek-v4.1-flash`, алиасы `claude-v4.1-flash` и `claude-v151-flash` → `deepseek-v4.1-flash`, `claude-sonnet-5` → `deepseek-v4.1-flash`.

- [ ] **Step 10: Commit**

```bash
git add go.mod config.go config_test.go config.json
git commit -m "feat: config loader with hot reload and atomic save"
```

---

### Task 2: Реестр моделей и резолвинг

**Files:**
- Create: `models.go`, `models_test.go`, `models.json`

**Interfaces:**
- Consumes: `ModelsConfig` из Task 1.
- Produces:
  - `type Model struct { ID, Picker, Label, BehavesAs, MaxEffort string; Supports1M, Available bool; Context int; Pricing Pricing }`
  - `type Pricing struct { Input, Output, CacheRead float64 }`
  - `type Registry struct { ... }`
  - `func LoadRegistry(path string) (*Registry, error)`
  - `func (r *Registry) Native(id string) (Model, bool)` — id приводится к нижнему регистру
  - `func (r *Registry) Resolve(model string, cfg ModelsConfig) (target, reason string)` — `reason` ∈ `alias|picker|background|native|default`
  - `func (r *Registry) Available() []Model` — только `Available`, в порядке файла

- [ ] **Step 1: Написать падающий табличный тест на резолвинг**

```go
func TestResolve(t *testing.T) {
    r := mustRegistry(t, `{"models":[
        {"id":"glm-5.3","picker":"claude-GLM-5.3","available":true},
        {"id":"claude-haiku-5-5","native":true,"available":true},
        {"id":"deepseek-v4.1-flash","picker":"claude-DeepSeek","available":true}]}`)
    cfg := ModelsConfig{
        Default: "deepseek-v4.1-flash", Background: "glm-5.3-flash",
        Aliases: map[string]string{"claude-v4.1-flash": "glm-5.3"},
    }
    cases := []struct{ in, want, why string }{
        {"claude-v4.1-flash", "glm-5.3", "alias"},
        {"CLAUDE-V4.1-FLASH", "glm-5.3", "alias"},
        {"claude-GLM-5.3", "glm-5.3", "picker"},
        {"claude-glm-5.3", "glm-5.3", "picker"},
        {"claude-haiku-5-5", "glm-5.3-flash", "background"},
        {"claude-haiku-4-5", "glm-5.3-flash", "background"},
        {"claude-claude-haiku-5-5", "claude-haiku-5-5", "picker"},
        {"deepseek-v4.1-flash", "deepseek-v4.1-flash", "native"},
        {"claude-sonnet-5", "deepseek-v4.1-flash", "default"},
        {"claude-opus-5", "deepseek-v4.1-flash", "default"},
        {"", "deepseek-v4.1-flash", "default"},
    }
    for _, c := range cases {
        got, why := r.Resolve(c.in, cfg)
        if got != c.want || why != c.why {
            t.Errorf("Resolve(%q) = (%q,%q), want (%q,%q)", c.in, got, why, c.want, c.why)
        }
    }
}
```

- [ ] **Step 2: Запустить — падает**

Run: `go test ./... -run TestResolve`
Expected: FAIL — `undefined: LoadRegistry`

- [ ] **Step 3: Реализовать `models.go`**

Порядок резолвинга ровно как в спеке: алиасы → `claude-<X>` где `<X>` есть в реестре → `claude-haiku-*` → native → default. Всё сравнение через `strings.ToLower`. Пустая строка модели — сразу `default`.

- [ ] **Step 4: Запустить тест — проходит**

Run: `go test ./... -run TestResolve -v`
Expected: PASS

- [ ] **Step 5: Тест на реестр и `Available()`**

```go
func TestAvailableFiltersAndPreservesOrder(t *testing.T) // три модели, одна unavailable -> Available() отдаёт две в порядке файла
func TestNativeIsCaseInsensitive(t *testing.T)           // Native("GLM-5.3") находит
```

- [ ] **Step 6: Реализовать остальное и прогнать**

Run: `go test ./... -run 'TestAvailable|TestNative|TestResolve' -v`
Expected: PASS

- [ ] **Step 7: Создать минимальный `models.json`**

Пока три записи для разработки: `deepseek-v4.1-flash`, `glm-5.3`, `claude-haiku-5-5`. Полный файл генерируется в Task 8.

- [ ] **Step 8: Commit**

```bash
git add models.go models_test.go models.json
git commit -m "feat: model registry and resolution rules"
```

---

### Task 3: Логгер с ротацией и захватом трафика

**Files:**
- Create: `log.go`, `log_test.go`

**Interfaces:**
- Consumes: `LogConfig`, `CaptureConfig` из Task 1.
- Produces:
  - `type RequestRecord struct { ID, Method, Path, Session, ModelIn, ModelOut, Why string; Status, Attempt int; Duration time.Duration; In, Out, CacheRead, Cost, Allowance float64; Aborted bool; Capture *CaptureRecord }`
  - `type CaptureRecord struct { RequestHeaders, ResponseHeaders map[string][]string; RequestBody, ResponseBody string; Truncated bool }`
  - `func NewLogger(cfg LogConfig) (*Logger, error)`
  - `func (l *Logger) Request(rec RequestRecord)`
  - `func (l *Logger) Close() error`
  - `func maskHeaders(h http.Header) map[string][]string`

- [ ] **Step 1: Падающий тест на формат строки**

```go
func TestRequestLineFormat(t *testing.T) {
    // записать в bytes.Buffer через новый логгер с io.Writer для теста
    rec := RequestRecord{ID:"a1", Method:"POST", Path:"/v1/messages", ModelIn:"claude-GLM-5.3",
        ModelOut:"glm-5.3", Why:"picker", Status:200, Duration:1240*time.Millisecond,
        In:14, Out:9, CacheRead:100, Cost:0.00042, Attempt:2}
    // ожидаем строку, содержащую: `req=a1`, `POST /v1/messages`, `model=claude-GLM-5.3->glm-5.3`,
    // `why=picker`, `status=200`, `dur=1.24s`, `in=14`, `out=9`, `cache_read=100`,
    // `cost=0.000420`, `attempt=2`
}
```

- [ ] **Step 2: Запустить — падает**

Run: `go test ./... -run TestRequestLineFormat`
Expected: FAIL — `undefined: NewLogger`

- [ ] **Step 3: Реализовать `Logger` и форматирование**

Строка: `2026-11-07T12:32:01+03:00 INFO  req=a1 POST /v1/messages session=… model=… why=… status=… dur=… in=… out=… cache_read=… cost=… attempt=…`. Пустые `why`/`session` не печатаются. `Duration` печатается через `%.2fs`.

- [ ] **Step 4: Прогнать — проходит**

Run: `go test ./... -run TestRequestLineFormat -v`
Expected: PASS

- [ ] **Step 5: Тесты на маскировку и захват**

```go
func TestMaskHeadersHidesCredentials(t *testing.T) // Authorization и x-api-key -> ["***"], остальные как есть
func TestCaptureJSONLWrittenOnlyWhenEnabled(t *testing.T) // Capture!=nil -> файл .jsonl, одна строка, валидный JSON
func TestCaptureBodyIsTruncated(t *testing.T)            // тело 100000 байт при max 65536 -> Truncated=true, len==65536
```

- [ ] **Step 6: Реализовать захват и ротацию**

`Request()` пишет основную строку, затем, если `rec.Capture != nil`, — объект в `<file>.jsonl` через `json.Encoder`. Ротация проверяется перед каждой записью: `os.Stat`, при превышении `MaxMB` — `os.Rename(file, file+".1")`, старые сдвигаются, лишние удаляются.

- [ ] **Step 7: Прогнать всё**

Run: `go test ./... -run 'TestRequest|TestMask|TestCapture' -v`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add log.go log_test.go
git commit -m "feat: structured request log with rotation and traffic capture"
```

---

### Task 4: Политика ретраев

**Files:**
- Create: `retry.go`, `retry_test.go`

**Interfaces:**
- Consumes: `RetryConfig` из Task 1.
- Produces:
  - `func ShouldRetry(status int) bool` — 429 и 500–599, никогда 4xx кроме 429
  - `func (p RetryPolicy) WaitFor(attempt int, retryAfter time.Duration) time.Duration` — `attempt` с 1
  - `func NewRetryPolicy(cfg RetryConfig, rnd *rand.Rand) RetryPolicy`
  - `func ParsePrefillHint(body []byte) time.Duration` — вытаскивает «after 30 s» только для лога

- [ ] **Step 1: Падающие тесты**

```go
func TestShouldRetry(t *testing.T) // 429 -> true, 500 -> true, 599 -> true, 400 -> false, 404 -> false, 401 -> false, 200 -> false
func TestWaitForGrows(t *testing.T) // attempt 1 -> [1600ms,2400ms]; attempt 3 -> [6400ms,9600ms] при джиттере ±20%
func TestWaitForCapsAtMax(t *testing.T) // attempt 6 -> не больше 15000 мс * 1.2
func TestWaitForHonoursRetryAfterWithinCap(t *testing.T) // retryAfter=40s -> 15000ms; retryAfter=3s -> [2400ms,3600ms]
func TestParsePrefillHint(t *testing.T) // тело с "(after 30 s)" -> 30*time.Second; мусор -> 0
```

- [ ] **Step 2: Запустить — падает**

Run: `go test ./... -run 'TestShouldRetry|TestWaitFor|TestParsePrefill'`
Expected: FAIL

- [ ] **Step 3: Реализовать `retry.go`**

Базовая задержка `FirstWaitMS * Multiplier^(attempt-1)`, потолок `MaxWaitMS`, джиттер ±20% от результата. Если `retryAfter > 0` — берём минимум из него и `MaxWaitMS`, тоже с джиттером.

- [ ] **Step 4: Прогнать**

Run: `go test ./... -run 'TestShouldRetry|TestWaitFor|TestParsePrefill' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add retry.go retry_test.go
git commit -m "feat: retry policy with jitter and retry-after cap"
```

---

### Task 5: Обработчик прокси

**Files:**
- Create: `proxy.go`, `proxy_test.go`

**Interfaces:**
- Consumes: `ConfigLoader`, `Registry`, `Logger`, `RetryPolicy`.
- Produces:
  - `type Proxy struct { ... }`
  - `func NewProxy(cfg *ConfigLoader, reg *Registry, log *Logger, client *http.Client) *Proxy`
  - `func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request)`
  - `func (p *Proxy) Stats() Stats`
  - `type Stats struct { Requests, Retries, Upstream429, InFlight int64 }`

- [ ] **Step 1: Тестовый апстрим и падающий тест на подмену модели**

```go
func TestRewritesModelBeforeSending(t *testing.T) {
    // httptest-сервер апстрима запоминает тело; реестр с glm-5.3, пикер claude-GLM-5.3
    // запрос POST /v1/messages с телом {"model":"claude-GLM-5.3","messages":[]}
    // ожидаем: апстрим получил model=glm-5.3, client видит 200 и тело апстрима
}

func TestNonJSONBodyPassesThrough(t *testing.T)      // тело "not json" -> апстрим получает те же байты, статус 200
func TestSessionHeaderSent(t *testing.T)             // апстрим видит x-opencode-session из X-Claude-Code-Session-Id
func TestSessionFallbackWhenHeaderMissing(t *testing.T)
```

- [ ] **Step 2: Запустить — падает**

Run: `go test ./... -run 'TestRewrites|TestNonJSON|TestSession'`
Expected: FAIL — `undefined: NewProxy`

- [ ] **Step 3: Реализовать пересылку и подмену**

Читаем тело целиком, `json.Valid` + `Unmarshal` в `map[string]any`, переписываем `model` через `Registry.Resolve`, `Marshal`. Ошибки чтения — 400. Заголовки: `Authorization`, `x-api-key`, `anthropic-version`, `content-type`, `user-agent`, `anthropic-beta` — копируем; `x-opencode-session` ставим всегда.

- [ ] **Step 4: Прогнать — проходит**

Run: `go test ./... -run 'TestRewrites|TestNonJSON|TestSession' -v`
Expected: PASS

- [ ] **Step 5: Падающие тесты на ретраи**

```go
func TestRetriesOn429ThenSucceeds(t *testing.T)   // апстрим: 429 один раз, потом 200; клиент видит 200; Attempt=2
func TestGivesUpAfterMaxAttempts(t *testing.T)    // апстрим всегда 429; клиент видит 429 и тело апстрима
func TestRetryHonoursTotalBudget(t *testing.T)    // конфиг budget=300ms; не ждём 5 попыток по 2с — тест идёт < 2с
func TestDoesNotRetry404(t *testing.T)            // апстрим 404 -> один запрос, клиент видит 404
```

Ретраи тестируются с `FirstWaitMS=10` через тестовый конфиг.

- [ ] **Step 6: Реализовать ретраи до отдачи заголовков**

Цикл: попытка → если `ShouldRetry(status)` и есть бюджет — читаем тело ошибки, ждём, повторяем. Заголовки клиенту пишем только после успешной попытки или исчерпания бюджета. Счётчики в `Stats`.

- [ ] **Step 7: Прогнать**

Run: `go test ./... -run 'TestRetries|TestGivesUp|TestDoesNotRetry' -v`
Expected: PASS

- [ ] **Step 8: Падающие тесты на стриминг, кэш и стоимость**

```go
func TestStreamsAndFlushesChunks(t *testing.T)  // апстрим пишет 3 чанка с задержкой; клиент получает их по мере записи
func TestCountTokensCacheHit(t *testing.T)      // два одинаковых count_tokens -> апстрим вызван один раз
func TestCountTokensSingleflight(t *testing.T)  // два параллельных одинаковых -> один вызов наверх (atomic счётчик)
func TestExtractsCostAndTokens(t *testing.T)    // usage.opencode.list_cost -> запись лога с cost
func TestClientDisconnectAborts(t *testing.T)   // клиент рвёт контекст -> апстрим-запрос отменён, строка с aborted=true
```

- [ ] **Step 9: Реализовать стриминг, кэш и извлечение стоимости**

Чтение буфером 32 КБ с `Flush` после каждой записи. Для `/v1/messages/count_tokens` — `sync.Map` с TTL 60 с и `singleflight`-подобной группировкой через `map[string]*inflight` под мьютексом. Для стоимости — для нестриминговых ответов разбираем тело, для стриминговых ищем последний JSON-объект с `usage`. Захват тела ответа — через `io.TeeReader` в буфер с обрезкой по `MaxBodyBytes`.

- [ ] **Step 10: Прогнать всё и закоммитить**

Run: `go test ./... -race -v`
Expected: PASS

```bash
git add proxy.go proxy_test.go
git commit -m "feat: proxy handler with retries, streaming, token cache and cost logging"
```

---

### Task 6: Админ-API и счётчики

**Files:**
- Create: `admin.go`, `admin_test.go`

**Interfaces:**
- Consumes: `Proxy`, `ConfigLoader`, `Registry`, `Logger`.
- Produces:
  - `func (p *Proxy) AdminHandler() http.Handler`
  - маршруты: `GET /admin/status`, `GET /admin/config`, `PATCH /admin/config`, `GET|PUT /admin/log/capture`, `GET /admin/models`, `PUT /admin/models/aliases`, `POST /admin/reload`

- [ ] **Step 1: Падающие тесты**

```go
func TestAdminStatusReportsCounters(t *testing.T)        // после двух запросов requests==2
func TestAdminPutCaptureTogglesAndPersists(t *testing.T) // PUT -> GET видит флаги; файл на диске содержит их
func TestAdminPatchConfigMergesNotReplaces(t *testing.T) // PATCH одного поля не стирает Models
func TestAdminAliasesReplace(t *testing.T)               // PUT заменяет таблицу целиком, старые исчезают
func TestAdminRequiresTokenWhenSet(t *testing.T)         // token задан -> без заголовка 401, с ним 200
func TestAdminDisabledReturns404(t *testing.T)           // enabled=false -> 404
```

- [ ] **Step 2: Запустить — падает**

Run: `go test ./... -run TestAdmin`
Expected: FAIL

- [ ] **Step 3: Реализовать `admin.go`**

`PATCH /admin/config` принимает частичный JSON и мержит по полям верхнего уровня (`log.capture` мержится по четырём ключам, `models.aliases` заменяется целиком). Ответ — эффективный конфиг после применения. Секреты (`admin.token`) в ответе заменяются на `***`.

- [ ] **Step 4: Прогнать**

Run: `go test ./... -run TestAdmin -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add admin.go admin_test.go
git commit -m "feat: admin REST API for logs, config and aliases"
```

---

### Task 7: Точка входа

**Files:**
- Modify: `main.go` (переписывается целиком)

**Interfaces:**
- Consumes: всё предыдущее.
- Produces: бинарник `opencode-proxy`; флаги `-config`, `-addr`.

- [ ] **Step 1: Падающий тест на разбор флага и выбор обработчика**

```go
func TestRouterSendsAdminToAdmin(t *testing.T)    // /admin/status не уходит наверх
func TestRouterSendsV1Upstream(t *testing.T)      // /v1/messages уходит наверх
```

- [ ] **Step 2: Реализовать `main.go`**

Флаги `-config` (по умолчанию `~/opencode-proxy/config.json`) и `-addr` (перекрывает `listen`). Загрузка конфига и реестра, `http.Client` с `Transport` и таймаутом соединения из конфига, маршрутизация `http.ServeMux`: `/admin/` → админка, всё остальное → прокси. `log.Fatalf` при ошибке загрузки.

- [ ] **Step 3: Собрать и прогнать тесты**

```bash
go build -o opencode-proxy . && go test ./... -race
```

Expected: сборка без ошибок, тесты PASS

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit -m "feat: wire config, registry, proxy and admin into main"
```

---

### Task 8: Генератор `models.json`

**Files:**
- Create: `cmd/genmodels/main.go`, `cmd/genmodels/main_test.go`

**Interfaces:**
- Consumes: `Model`, `Pricing` из Task 2.
- Produces:
  - `func FetchCatalog(ctx context.Context, url string, hc *http.Client) ([]Model, error)`
  - `func Probe(ctx context.Context, baseURL, token string, m Model, hc *http.Client) (available bool, err error)`
  - `cmd/genmodels` пишет `models.json` с флагами `-out`, `-probe`, `-token-file`.

- [ ] **Step 1: Падающие тесты на разбор каталога**

```go
func TestParseCatalog(t *testing.T)      // фикстура с одной моделью -> Model{ID, Label, Context, MaxEffort из последнего supported, Pricing}
func TestPickerNameFromID(t *testing.T)  // "glm-5.3-flash" -> "claude-GLM-5.3-Flash"
func TestMaxEffortPicksHighestSupported(t *testing.T) // effort ["low","high","max"] -> "max"
func TestUnavailableWhenProbeFails(t *testing.T)      // Probe возвращает 429 -> available=false
```

- [ ] **Step 2: Реализовать и прогнать**

Run: `go test ./cmd/genmodels/... -v`
Expected: PASS

- [ ] **Step 3: Сгенерировать реальный `models.json`**

```bash
go run ./cmd/genmodels -out models.json -probe -token-file ~/.claude/settings.json
```

Ожидается ~30 записей, у 21 `available: true` (`gpt-*`, `grok-4.7`, `muse-spark-*`, `beam-free`, `crouton-alpha-free` — false; `longcat-2.0` — false; `iceman-contributor-free` — false).

- [ ] **Step 4: Commit**

```bash
git add cmd/genmodels models.json
git commit -m "feat: generate model registry from OpenCode catalog"
```

---

### Task 9: Навык `claude-config`

**Files:**
- Create: `.claude/skills/claude-config/SKILL.md`

**Interfaces:**
- Consumes: `models.json`.
- Produces: навык, который читает `models.json`, собирает `inferenceModels` и правит активный профиль.

- [ ] **Step 1: Написать `SKILL.md`**

Frontmatter: `name: claude-config`, `description` — когда применять (перегенерировать конфиг, добавить модели, обновить после смены каталога). Тело описывает:

1. Прочитать `models.json`, взять только `available: true`.
2. Прочитать `~/Library/Application Support/Claude-3p/configLibrary/_meta.json`, взять `appliedId`.
3. Собрать записи: `name` = `picker`, `labelOverride` = `label`, `behavesAs` = `behaves_as`, `maxEffort` = `max_effort`, `supports1m` = `supports_1m`.
4. Показать diff против текущего файла.
5. После подтверждения — бэкап `<id>.json.bak` и запись.
6. Напомнить перезапустить приложение.

Обязательный запрет в тексте навыка: не писать без показа diff'а, не трогать чужие ключи профиля.

- [ ] **Step 2: Проверить навык вручную**

Попросить навык показать diff, не применяя. Убедиться, что в diff ровно 21 запись и ключи профиля не задеты.

- [ ] **Step 3: Commit**

```bash
git add .claude/skills/claude-config/SKILL.md
git commit -m "feat: skill to generate Claude Desktop model config"
```

---

### Task 10: Сервис и живая проверка

**Files:**
- Create: `deploy/ai.opencode.proxy.plist`, `Makefile`

**Interfaces:**
- Consumes: собранный бинарник.
- Produces: цели `make build`, `make install`, `make uninstall`, `make verify`.

- [ ] **Step 1: Написать plist и Makefile**

Plist: `Label` = `ai.opencode.proxy`, `ProgramArguments` = абсолютный путь к бинарнику и `-config`, `RunAtLoad`, `KeepAlive`, `StandardOutPath`/`StandardErrorPath` в `~/opencode-proxy/`, `WorkingDirectory` — каталог проекта.

- [ ] **Step 2: Установить и проверить, что живёт**

```bash
make build && make install
launchctl print gui/$(id -u)/ai.opencode.proxy | head -20
```

Ожидается: `state = running`.

- [ ] **Step 3: Живая проверка через админку**

```bash
curl -s localhost:4000/admin/status | jq
curl -s localhost:4000/admin/models | jq '.models | length'
```

Ожидается: `upstream` = `https://opencode.ai/inference/go/anthropic`, число моделей = 30.

- [ ] **Step 4: Живая проверка подмены модели**

```bash
TOKEN=$(jq -r '.env.ANTHROPIC_AUTH_TOKEN' ~/.claude/settings.json)
curl -s localhost:4000/v1/messages \
  -H "x-api-key: $TOKEN" -H "anthropic-version: 2023-06-01" -H "content-type: application/json" \
  -d '{"model":"claude-GLM-5.3","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}' | jq '.usage.opencode'
```

Ожидается: ответ 200 и блок `opencode` с `list_cost`.

- [ ] **Step 5: Проверить захват логов на ходу**

```bash
curl -s -X PUT localhost:4000/admin/log/capture -d '{"request_headers":true}'
curl -s localhost:4000/admin/log/capture | jq
tail -1 ~/opencode-proxy/proxy.log.jsonl | jq 'keys'
```

Ожидается: в `.jsonl` есть `request_headers`, при этом `Authorization` и `x-api-key` заменены на `***`.

- [ ] **Step 6: Commit**

```bash
git add deploy Makefile
git commit -m "feat: launchd service and make targets"
```

---

## Self-Review

**Spec coverage:** конфиг (T1), резолвинг (T2), логи и захват (T3), ретраи (T4), прокси/стриминг/кэш/стоимость (T5), админка (T6), точка входа (T7), каталог моделей (T8), навык (T9), сервис и проверка (T10). Все разделы спеки 1–9 покрыты.

**Открытый вопрос спеки — ошибка внутри SSE-стрима.** Task 5 реализует ретрай только по статусу. Если живая проверка Task 10 покажет, что `prefill_cap_refused` приходит ивентом внутри стрима, добавляется отдельная задача «подсмотр первого SSE-ивента» — до этого момента она не нужна и не планируется.

**Type consistency:** `Model`/`Pricing` определены в T2 и используются в T8; `RequestRecord` определён в T3 и заполняется в T5/T6; `Stats` — в T5, читается в T6.
