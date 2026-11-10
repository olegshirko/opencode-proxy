# Qoder как второй upstream в opencode-proxy — дизайн

Дата: 2026-11-10
Статус: черновик на согласование
Источник фактов: реверс Qoder.app 0.5.7 (macOS arm64), артефакты в `var/qoder-research/`
(распакованный asar, декодер `tools/decode2.mjs`, таблица строк `table-decoded.txt`),
сверено с живыми логами (`~/Library/Application Support/com.qoder.app.stable/logs/`,
`~/.qoder/logs/runs/`). Схема протокола — `proto/chat.proto` из воркера приложения.

## Замысел и почему

opencode-proxy сейчас говорит только с OpenCode Go. Qoder — вторая подписка с другим
набором моделей; хочется видеть её модели в том же пикере Claude Code/Desktop за тем же
прокси на 127.0.0.1:4000, с тем же ретраем, логированием и учётом расходов. Клиент
(Claude Code CLI / Desktop) продолжает говорить по Anthropic Messages API — прокси
транслирует.

## Цели

- Модели подписки Qoder доступны через тот же `POST /v1/messages` и `/v1/models`.
- Авторизация полностью внутри прокси: пользователь один раз вставляет Personal Access
  Token (PAT), прокси сам обменивает его на jobToken и рефрешит.
- Учёт расходов/квот Qoder — по возможностям их API (не обязательно в первой фазе).

## Не-цели

- Не расшифровывать `auth.v1.dat` приложения и не трогать Keychain.
- Не эмулировать агентный протокол Qoder (tool-циклы воркера) — только chat completions.
- Не поддерживать protobuf/gRPC-транспорт `agent_chat_generation` в первой фазе —
  только JSON-путь `/model/v1/chat/completions`.

## Анти-цели (что считаем провалом)

- Хранение PAT или jobToken в логах/_capture (секреты маскируются как для
  Authorization, так и в новых полях конфига).
- Поломка существующего OpenCode-пути: без конфига Qoder поведение прокси не меняется.
- Молчаливая подмена модели: выбранная claude-* алиасом модель Qoder резолвится
  явно и логируется с причиной, как и сейчас.

## Факты о стороне Qoder (проверено)

### Аутентификация

- PAT создаётся в приложении: Account → Integrations (docs.qoder.com/cli/sdk/authentication).
- Обмен: `POST {openapi}/api/v1/jobToken/exchange`, JSON-тело
  `{personal_token: "<PAT>"}` (+ опционально machine identity). Ответ:
  `{token | device_token | access_token}` + поля сроков
  (`expire_time|expires_at`, `expires_in`, `refresh_token_expire_time|refresh_token_expires_at`,
  `refresh_token_expires_in`) и `refresh_token`.
- Рефреш: `POST {openapi}/api/v1/jobToken/refresh`, тело `{refresh_token}` — тот же
  формат ответа. Код воркера: константы `Zyg/Xyg`, класс `_Wt` (jobToken) и
  класс deviceToken (`Vyg/qyg`) — устройство идентично.
- openapi-хост: `https://openapi.qoder.sh` (глобальный регион; CN-вариант
  `openapi.qoder.com.cn`). CLI при старте делает election
  `GET https://center.qoder.sh/api/v3/service/region/endpoints`, но для нашей цели
  фиксированный `.sh`-хост достаточен; вынесем в конфиг.

### Инференс (JSON-путь, подтверждён кодом `openResponse`)

```
POST https://api1.qoder.sh/model/v1/chat/completions
Accept: text/event-stream
Content-Type: application/json
Authorization: Bearer <jobToken или access_token — проверить с живым PAT>
X-Request-ID: <uuid>
X-Session-ID: <uuid>
```

Тело — OpenAI Chat Completions + qoder-обвязка (функция `QFn` воркера):

```json
{
  "model": "<id из каталога>",
  "messages": [{"role": "...", "content": "..." | [...parts]}],
  "stream": true,
  "stream_options": {"include_usage": true},
  "metadata": {"context": {"request_id": "...", "session_id": "...",
               "client_type": "..."}},
  "tools": [], "stop": ..., "max_tokens": ..., "temperature": ...,
  "reasoning_effort": "..."
}
```

Ответ — SSE `text/event-stream`; чанки в OpenAI-формате
(`{id, object, created, model, choices:[{index, delta:{role, content, reasoning_content,
tool_calls}, finish_reason}], usage}`) — соответствует `ChatCompletionChunk` из
`chat.proto`. Финализация запроса у клиента — `POST /algo/api/v2/service/business/finish`
(биллинг; в первой фазе не шлём и смотрим, не влияет ли на учёт квот).

- Каталог моделей: `GET https://api1.qoder.sh/api/v2/model/list` (без auth в логах не
  видно; проверим с живым токеном).
- Квоты: `GET https://center.qoder.sh/sash/api/v2/me/usage` и
  `/api/v2/quota/usage` (вторая — из воркера). Формат проверить живым вызовом.
- Оверрайд хоста инференса: env `QODER_MODEL_SERVER_HOST` у воркера; у нас — конфиг.

### Что остаётся проверить живым PAT (первый шаг реализации)

1. Работает ли обмен PAT → jobToken с пустым machine identity; формат и срок жизни.
2. Какой именно Bearer ест `/model/v1/chat/completions` (jobToken против
   пользовательского access_token: код берёт `security_oauth_token ?? access_token`,
   но daemon ходил с host-job-token).
3. Живой `/api/v2/model/list`: ids моделей, доступные на плане `personal_standard`.
4. Реальные чанки SSE JSON-пути (поля delta, usage, reasoning_content).
5. Обязателен ли `business/finish` для корректного учёта квоты.

## Интеграция в прокси

Существующие точки уже подходят: роут с собственным `base_url` и кредами
(`route.Token()`, [proxy.go:484](../../../internal/proxy/proxy.go)) и запрет
молчаливой подмены модели. Новое:

1. **`internal/qoderauth`** — token manager: exchange → in-memory jobToken+refresh с
   `expiresAt`, refresh за N секунд до истечения, re-exchange при 401/invalid_token,
   мьютекс против лавины рефрешей. PAT берёт из `config.qoder.token_file`/`token_env`
   (как у роутов). Персистентность состояния не нужна — exchange дешёвый.
2. **`internal/qoderchat`** — транслятор Anthropic Messages ⇄ Qoder chat completions:
   - request: `system` → system-message, `tools` → OpenAI tools, `tool_use`/`tool_result`
     → `tool_calls`/`tool`, multimodal-части → `{type:"image_url"}`-части;
     `metadata.context` из session/request id прокси.
   - response: OpenAI SSE-чанки → Anthropic SSE (`message_start`, `content_block_delta`,
     `message_delta` c stop_reason, `message_stop`); `reasoning_content` → thinking-блок
     (в стиль существующего `thinking`-пакета); usage → существующий coststore/quota.
3. **Роут**: новый `route.kind = "qoder"`: `base_url = https://api1.qoder.sh`,
   upstream-path `/model/v1/chat/completions`, креды — не статичный токен, а
   `qoderauth`. Резолвер моделей: новые native id из каталога Qoder; `/v1/models`
   расширяется; `models.json`/пикер перегенерировать `claude-config`-скиллом.
4. **Конфиг** (`config.json`, hot reload): секция `qoder`:
   `{enabled, openapi_base_url, inference_base_url, token_file|token_env, model ids...}`.
5. **Ретраи**: 401/403 с токеном — один re-auth и повтор, остальные по существующей
   лестнице; `429` — как сейчас.

## Фазы

1. **Verification spike** (без изменений прокси): скрипт в `var/qoder-research/` с
   PAT: exchange, model list, один мини-запрос в completions, дамп чанков. Выход —
   зафиксированный контракт в этом документе.
2. **`qoderauth` + route kind** с одним смоук-моделью, тесты на рефреш/401.
3. **Транслятор** полный (tools, multimodal, thinking), тесты на золотых парах.
4. **Пикер/каталог** и документация README.

## Решения по итогам builder-check

Пять вопросов проверки строителем; ниже ответы — мои решения (можно ветировать),
если не указано иное. Всё, что документ оставляет спайку с PAT, остаётся в списке
«проверить живым PAT».

1. **Реестр моделей и пикер.** Статические записи в `models.json` (сценарий-генератор
   после спайка, по образцу `make models`); живой фетч каталога Qoder в рантайме —
   не в фазе 1. Неймспейс пикера `claude-qoder-<native-id>`; дубликат пикер-имени —
   ошибка конфигурации на старте (никаких молчаливых перекрытий alias → picker).
   `models.default`/`background` не меняются: фоновый и дефолтный трафик остаётся на
   OpenCode, Qoder — только явно выбранные модели.
2. **Цены и учёт.** Pricing/context — из `model/list`, если отдаёт; иначе руками в
   конфиг (нули допустимы). Qoder-трафик попадает в coststore (виден в статус-лайне
   и `/admin/session-cost`), но **не** в лимитные окна `limits.*` — те остаются
   бюджетом OpenCode. Отдельная квота Qoder (`/sash/api/v2/me/usage`) — отдельная
   работа после первой фазы.
3. **Поверхность транслятора.** Non-streaming (`stream:false` от клиента) —
   накапливаем upstream-SSE и отдаём один Anthropic-JSON. `count_tokens` на
   Qoder-роутах — локальная эвристическая оценка (приближение, логируется как
   approximate). `cache_control` и `anthropic-beta`-заголовки срезаются.
   `tool_choice`: маппятся `auto`/`any`/конкретный tool; неизвестные варианты —
   явная ошибка трансляции, не молчаливая подмена.
4. **Thinking.** `reasoning_effort` запрашивается только когда клиент попросил
   thinking; `budget_tokens` → effort по порогам-константам (зафиксированы в коде
   транслятора); входящие thinking-блоки распознаются и срезаются на входе;
   модель без reasoning — игнор с записью в reqlog.
5. **Шедулер.** Один инстанс, ячейки с префиксом провайдера (`opencode:`/`qoder:`) —
   раздельные потолки, зависший Qoder-запрос не занимает ячейку OpenCode.
   Обучение prefill-потолка кормят только ответы OpenCode; main-приоритет общий.

## Ограничения

- PAT — секрет уровня подписки; в config не пишем, только файл/env вне git.
- Контракты Qoder не публичные и могут измениться без предупреждения; все
  предположения локализованы в `qoderauth`/`qoderchat`.
- Регион: `.sh`-хосты могут быть недоступны из некоторых сетей — оставить оба базовых
  URL конфигурируемыми.
