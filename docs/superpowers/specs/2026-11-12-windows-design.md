# Windows: фаза 2 — сервис, пути, упаковка

Дата: 2026-11-12
Дополнение к `docs/superpowers/specs/2026-11-09-cross-platform-design.md`
(его последняя секция — открытые вопросы этой фазы).

## Зачем

Код уже переносим: `GOOS=windows go build` проходит, CI-матрица гоняет тесты на
windows-latest, и они зелёные (проверено 2026-11-12). Не переносима обвязка:
make + sh-скрипты, launchd/systemd, XDG-пути, POSIX-сигналы. Windows-пользователь
Claude Code CLI сегодня не может поставить прокси по README.

## Цели

1. Чужой человек на Windows 10/11: `git clone` → одна команда установки →
   работающий прокси с автозапуском при логине и самовосстановлением, пикер
   и статуслайн в Claude Code CLI.
2. Без админа и без сторонних бинарников в базовом сценарии.
3. Поведение macOS/Linux не меняется.
4. CI: windows-latest становится воротами (не `continue-on-error`), smoke
   гоняется и на Windows.
5. Desktop-приложение — по результату живой разведки путей; CLI её не ждёт.

## Non-goals

- WSL — это уже покрытый Linux-путь.
- Настоящий Windows-сервис (свой svc-протокол через `x/sys/windows/svc` или
  враппер) — см. решение 1.
- Автообновление прокси.
- Подпись бинарника: он собирается локально из исходников, не скачивается.

## Anti-goals

- Требовать админа без причины: прокси слушает loopback и живёт в профиле
  пользователя.
- Резать стримы при рестарте — дрейн как на POSIX.
- Обещать в README desktop-поддержку до живой проверки путей.
- Молча менять пути или поведение существующих macOS/Linux-установок.

## Решения

### 1. Сервис-менеджер: Task Scheduler, не Windows-сервис

| Вариант | Админ | Зависимости | Сессия | Вердикт |
|---|---|---|---|---|
| `sc.exe` напрямую | да | нет | 0 | Не работает: Go-бинарник не говорит service control protocol, SCM отвечает 1053 (timeout). Нужен враппер или `x/sys/windows/svc` — ломает no-deps. |
| Startup-ярлык | нет | нет | пользовательская | Нет рестартов после падения, мигает консольное окно. |
| NSSM | да | сторонний exe | 0 | Последний релиз 2.24 (2014), проект не мейнтейнится. |
| WinSW 3 | да | сторонний exe + .NET Framework 4.6.1 (или self-contained-сборка) | 0 | Живой проект, но тащить враппер ради loopback-прокси — лишнее звено. |
| **Task Scheduler** | **нет** | **встроен** | **пользовательская** | **Рекомендация.** |

Семантика launchd `KeepAlive` / systemd `Restart=always` средствами планировщика:

- триггер AtLogOn + повтор каждые 5 минут бесконечно;
- MultipleInstances `IgnoreNew`: повтор при живом процессе ничего не делает —
  самовосстановление ≤5 минут после любого падения, без дублей;
- RestartOnFailure (PT1M, 3 попытки): быстрый рестарт после ненулевого выхода;
- `ExecutionTimeLimit = 0` — дефолтные 72 часа убили бы прокси, обязательно снять;
- `DisallowStartIfOnBatteries` / `StopIfGoingOnBatteries` = false — ноутбук;
- скрытый запуск, без окна.

Регистрация — `schtasks /Create /XML` с шаблоном
`deploy/opencode-proxy.task.xml.template` (плейсхолдеры `__HOME__`, `__BIN__`,
как в plist/systemd-юнитах): один артефакт, вся семантика ревьюится в diff.
PowerShell `New-ScheduledTask*` — запасной путь, если XML упрётся в лимиты схемы.

Почему не сервис в сессии 0: прокси читает токен из
`%USERPROFILE%\.claude\settings.json` и пишет состояние в профиль пользователя;
пользовательская сессия даёт это без настройки сервисных аккаунтов. Сервис
до логина не нужен: клиенты появляются только после логина.

### 2. Пути по умолчанию

- конфиг: `%APPDATA%\opencode-proxy\config.json`;
- состояние: `%LOCALAPPDATA%\opencode-proxy\var\`;
- бинарники: `%LOCALAPPDATA%\Programs\opencode-proxy\`
  (`opencode-proxy.exe`, `genmodels.exe`);
- `paths.go`: ветка `windows` в `defaultConfigPathFor`/`defaultStateDirFor`
  через `os.Getenv("APPDATA"/"LOCALAPPDATA")`; пустые переменные (обрезанное
  окружение) — фолбэк на домашний каталог. Табличный тест фиксирует
  darwin/linux/windows и фолбэк;
- явный `-config` перекрывает всё, как сейчас;
- токен-файл для probe: `%USERPROFILE%\.claude\settings.json`
  (`os.UserHomeDir` уже даёт это).

### 3. Упаковка: PowerShell-скрипты вместо make

GNU make на Windows нет; требовать scoop/choco — лишняя зависимость для чужого
человека. Make-цели зеркалятся скриптами (make на macOS/Linux не трогаем):

- `scripts/install.ps1`: `go test ./...` → `go build` → `smoke.ps1` →
  копирование бинарников → `models.json` в конфиг-каталог, если его нет →
  регистрация задачи. Ничего не заменяется, пока кандидат не ответил —
  как `make install`.
- `scripts/smoke.ps1`: зеркало `smoke.sh` — кандидат на порту 4010 с боевым
  конфигом, поллинг `/admin/status` до `ok:true`, `try/finally Stop-Process`.
- `scripts/restart.ps1`: `wait_idle` (`in_flight:0`) → грейсфул-стоп
  (решение 4) → `schtasks /Run` → `wait_healthy`.
- `scripts/uninstall.ps1`: снять задачу, удалить бинарники; конфиг и состояние
  остаются (как на POSIX).

README даёт запуск `powershell -ExecutionPolicy Bypass -File scripts\install.ps1`
(git clone не ставит Mark-of-the-Web, но флаг снимает вопросы политик).

### 4. Грейсфул-стоп: `POST /admin/shutdown`

Проблема: SIGTERM на Windows не доставляется; скрытый процесс без консоли не
получает CTRL_C; `schtasks /End` — это TerminateProcess: режет стримы и теряет
последний flush сторов.

Решение: один новый эндпоинт `POST /admin/shutdown` (только loopback,
`X-Admin-Token`, если задан) выдёргивает тот же drain-путь, что SIGTERM —
общий shutdown-канал в `main.go`. Ответ 200 сразу; процесс дорабатывает
in-flight ≤30 с, сбрасывает сторы и выходит с кодом 0.

- `restart.ps1`: `wait_idle` → POST → дождаться выхода процесса → `/Run`.
- Кроссплатформенный: httptest-тест на всех ОС. POSIX-скрипты не трогаем
  (могут перейти на эндпоинт позже — вне скоупа).

Альтернатива «принять жёсткий kill» отвергнута: это anti-goal.

### 5. Статуслайн

CLI-режим (bare session id) уже платформенный — работает без изменений.
Desktop `local_…` ids требуют Windows-аналога
`~/Library/Application Support/Claude-3p/claude-code-sessions` — живая разведка
(вопрос 2). Нет каталога — desktop-статуслайн откладывается, CLI не блокируется.

### 6. Desktop-профиль — research-gated

Кандидат путей: `%APPDATA%\Claude-3p\configLibrary\` (по аналогии с macOS).
Существует ли Windows-сборка с gateway-профилями вообще — проверить живьём
(вопрос 1). Порог решения: нашли `configLibrary` и профиль применяется →
скилл `claude-config` получает Windows-ветку; нет → фаза закрывается
CLI-полнотой, в матрице поддержки desktop честно «нет».

### 7. CI

- windows-latest: убрать `continue-on-error` (тесты зелёные уже сейчас).
- Добавить прогон `smoke.ps1` в windows-джоб: кандидат собирается и отвечает.
- `-race` на Windows не гоняем (нужен cgo) — как сейчас.

### 8. README

Матрица поддержки и Quick start Windows появляются только после живой приёмки
на Windows-машине. EN+RU зеркально.

## Открытые вопросы (живые проверки на Windows-машине)

1. **Есть ли Windows-сборка Claude Desktop с gateway-профилями?** Порог:
   `configLibrary` найден, профиль с `inferenceGatewayBaseUrl` применяется →
   desktop в скоупе; иначе CLI-only. Ответ фиксируется в этом документе.
2. **Каталог сессий desktop под Windows** (для `local_` → `cliSessionId`).
   Тот же порог: найден и маппинг сходится с логом прокси → статуслайн
   desktop делаем; иначе откладываем.
3. **Повтор задачи при живом процессе**: `IgnoreNew` не плодит копий; после
   kill процесс поднимается ≤5 минут. Проверяется до написания install.ps1.
4. **Дефолты задачи**: `ExecutionTimeLimit` (72 ч), battery-ограничения —
   зафиксировать в XML-шаблоне и проверить переживанием сна/батареи.
5. **Чистая машина без cgo**: `go test` уже зелёный в CI — дополнительный
   сигнал не нужен, но smoke.ps1 в CI закрывает пробел.

## Критерии приёмки

- Чистая Windows 10/11: clone → `install.ps1` → `/admin/status` ok; переживает
  logoff/logon; после kill поднимается ≤5 минут; `restart.ps1` не режет активный
  стрим (проверка при `in_flight > 0`).
- `uninstall.ps1` снимает задачу и бинарники, конфиг остаётся.
- macOS/Linux: make-цели и поведение без изменений (`make -n` diff).
- CI: windows — ворота, зелёные; `smoke.ps1` гоняется в CI.
- Статуслайн CLI показывает реальную стоимость сессии на Windows.

## Оценка объёма

| Кусок | Оценка |
|---|---|
| `paths.go` + табличные тесты | ~1 ч |
| `/admin/shutdown` + тесты | ~1–2 ч |
| 4 ps1 + XML-шаблон задачи | ~3–4 ч |
| CI + README (после приёмки) | ~1 ч |
| Разведка desktop (живая машина) | ~2–4 ч |

CLI-полнота: ~день. Desktop — сверху, по результату разведки. План реализации —
отдельным документом в `plans/` после ревью этой спеки.