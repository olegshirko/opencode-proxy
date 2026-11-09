# Кросс-платформенная поддержка: Linux, затем Windows

Дата: 2026-11-09
Дополнение к `docs/2026-11-07-opencode-proxy-design.md`.

## Зачем

Репозиторий публичный, и чужой человек должен уметь поднять прокси на своей машине.
Сегодня всё работает только на macOS: код прокси — чистый Go и уже собирается под
Linux и Windows (проверено `GOOS=linux/windows go build` 2026-11-09), но обвязка —
launchd, Makefile, статус-лайн, путь конфига — прибита к macOS. Linux-аудитория —
пользователи Claude Code CLI (официального desktop-приложения под Linux нет), и им
прокси даёт главное: 30 моделей каталога OpenCode Go в `/model` вместо одной.

Windows — вторая фаза, сознательно отдельная: там свои сервис-менеджеры и пути
desktop-приложения, которые надо исследовать; смешивать в один заход не стоит.

## Цели

1. Чужой человек на Ubuntu-подобном Linux от `git clone` до работающего прокси с
   пикером на 30 моделей в Claude Code CLI — по инструкциям из README, без чтения
   диздоков.
2. `make install` / `make restart` / `make uninstall` работают и на macOS, и на Linux
   (ветка по ОС), поведение на macOS не меняется.
3. Статус-лайн работает в CLI-режиме на любой платформе.
4. CI: тесты гоняются на ubuntu, macos, windows на каждый push (репозиторий сейчас
   без CI).

## Non-goals

- Desktop-приложение под Linux — его не существует.
- Прямая интеграция с провайдерами поиска (exa/tavily/...) в MCP — отдельная фаза;
  сейчас MCP остаётся мостом к локальному `opencode api`, зависимость от OpenCode CLI
  документируется.
- Windows — вне этого документа, кроме списка открытых вопросов (ниже).
- Автообновление прокси.

## Anti-goals

- Установка, требующая root без причины: на Linux прокси слушает loopback и живёт в
  домашнем каталоге — systemd **user**-юнит, не system.
- Молчаливая смена путей на macOS: существующие установки (этот Mac) не переезжают.
- README, обещающий Windows до того, как он проверен.

## Решения

### 1. Пути по умолчанию

- **macOS — не трогаем**: `~/opencode-proxy/config.json`, состояние в
  `~/opencode-proxy/var/`. Существующая установка продолжает жить.
- **Linux — XDG**: конфиг `~/.config/opencode-proxy/config.json`, состояние
  `~/.local/state/opencode-proxy/var/`. Каталоги создаются при первом запуске.
- Явный `-config` перекрывает всё, как сейчас.
- Реализация: `defaultConfigPath()` и `Defaults()` ветвятся по `runtime.GOOS`.
  Тест фиксирует три платформенных значения.

### 2. Сервис: systemd user-юнит

`deploy/opencode-proxy.service` (шаблон с `__HOME__`):

- `ExecStart=%h/.local/bin/opencode-proxy -config %h/.config/opencode-proxy/config.json`
  (бинарь кладёт `make install-linux` в `~/.local/bin`, он в PATH у большинства
  дистрибутивов);
- `Restart=always`, `RestartSec=5`;
- `WantedBy=default.target`; для работы без залогиненной сессии — документированный
  `loginctl enable-linger`.

Makefile: `install-linux`, `restart-linux`, `uninstall-linux` через
`systemctl --user`; корневые `install`/`restart`/`uninstall` ветвятся по
`uname -s`. `make build`, `test`, `smoke`, `verify`, `models`, `timer` уже
платформо-нейтральны (smoke.sh — чистый sh; `timer` на Linux пока не ставится —
это systemd timer, отдельная маленькая задача после юнита).

### 3. Статус-лайн: CLI-режим

Вход статус-лайна различается: desktop-приложение шлёт `session_id` вида `local_…`
(нужен маппинг через каталоги Claude-3p — остаётся macOS-веткой), CLI шлёт свой
id сессии напрямую — это тот же `X-Claude-Code-Session-Id`, что видит прокси.
Логика: id без префикса `local_` → запрос в `/admin/session-cost` без маппинга;
с префиксом → платформенный путь каталога (сейчас macOS; Windows — фаза 2).
CLI-режим тестируется юнит-тестом без файловой системы приложения.

### 4. Пикер моделей в CLI

Desktop-профиль под Linux не существует, но Claude Code CLI читает секцию
`modelPicker` из `~/.claude/settings.json`. Схема проверена по документации
окружения (2026-11-09): `modelPicker.options[]` — `{model, label, description,
behavesAs}`, где `model` — тот же id, что принимает `--model` (подойдут
пикер-имена вида `claude-GLM-5.3-Flash`), плюс `replaceBuiltInOptions: true`,
чтобы показать только наши модели. Генератор (`cmd/genmodels`, флаг
`-settings`) выпускает готовый блок: `ANTHROPIC_BASE_URL`, переменные окружения
с токеном (подсказка, куда вставить) и `modelPicker`. В пикер попадают только
`available: true` — как в desktop-профиле; чужому региону README советует один
раз прогнать `make models` (зонд по его региону). Решение строительское:
ошибка RegionError на недоступной модели читаема и мгновенна, а молча
отсутствующие модели — нет.

### 5. Первый запуск без конфига

Отдельный вопрос ревьюера: сегодня `NewConfigLoader` фаталит без файла, а
tracked `config.json` — личный живой конфиг автора с абсолютными путями.
Решение: если файла нет, загрузчик сам создаёт его из `Defaults()` и продолжает
(на macOS ничего не меняется — файл там всегда есть). `models.json` при
отсутствии не фаталит: прокси стартует с пустым реестром (резолвинг держит
живой каталог) и пишет подсказку в лог. `make install` кладёт `models.json` из
репозитория в конфиг-каталог, если его там нет; `make models` пишет в
конфиг-каталог активной платформы (`$(CONFIG_DIR)/models.json`), чтобы
ночная перегенерация доходила до прокси и на Linux. Tracked `config.json`
остаётся рабочим файлом автора; README на него как источник не ссылается.

### 6. CI

`.github/workflows/test.yml`: матрица `[ubuntu-latest, macos-latest, windows-latest]`,
`go test -race ./...`. windows-latest — `continue-on-error: true`: в фазе 1 это
сигнал о переносимости, а не ворота; воротами он станет в фазе 2, когда
Windows-поведение будет осознанным. Бонус, не треба Linux: закрывает
отсутствие CI вообще.

### 7. Статус-лайн: проверка контракта живьём

Допущение «CLI-`session_id` == `X-Claude-Code-Session-Id`» в плане проверяется
первым шагом задачи статус-лайна (один прогон CLI через прокси со сравнением
id в логе), до написания CLI-ветки — если допущение неверно, задача
переформулируется, а не продавливается.

### 8. README

Разделы Quick start разбиваются на вкладки-подзаголовки «macOS» / «Linux» (Windows —
строка «coming»), Linux-путь: `git clone` → `make install` (или `install-linux`)
→ блок settings.json → опционально статус-лайн и поиск. Строка «macOS only» в шапке
меняется на точную матрицу поддержки.

## Что остаётся решать строителю

- Точные имена make-целей и содержимое юнита (EnvironmentFile не нужен, но уточнить
  `RestartSec` и `MemoryMax`).
- Формат `-settings`-вывода генератора: единый JSON-блок vs два отдельных поля.
- `PKGS` для `go vet` в CI (нет go.sum — модуль без зависимостей).

## Фаза 2: Windows (отдельный дизайн, не в этом документе)

- Сервис-менеджер: NSSM vs WinSW vs `sc.exe` + планировщик.
- Пути Windows-версии Claude Desktop (configLibrary, каталог сессий для статус-лайна).
- `claude-config` и `genmodels -settings` под `%APPDATA%`.
- Проверка smoke.sh под PowerShell или его замена.
- CI на windows-latest уже будет гонять тесты — это первый сигнал о переносимости.
