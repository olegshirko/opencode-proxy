---
name: claude-config
description: Regenerate the Claude Desktop gateway profile from models.json — the model picker entries and per-model pricing. Use when the model list changes, when a model is added or disappears from OpenCode Go, when the picker shows wrong prices or a wrong context window, or when the user says "обнови конфиг", "перегенерируй пикер", "модели не появились".
---

# Генерация конфига Claude Desktop

Профиль шлюза в приложении — это файл в
`~/Library/Application Support/Claude-3p/configLibrary/<appliedId>.json`.
Руками его править не нужно: всё, что относится к моделям и ценам,
собирается из `models.json` в корне проекта.

## Что делать

1. Убедись, что `models.json` свежий. Если модели менялись:
   `go run ./cmd/genmodels -out models.json -probe -token-file ~/.claude/settings.json`
   Без `-probe` все модели помечаются доступными, и в пикер попадут те,
   которые в твоём регионе отдают `RegionError`.

2. Покажи diff, ничего не записывая:

   ```
   python3 .claude/skills/claude-config/generate.py
   ```

3. Покажи его пользователю и дождись подтверждения. Diff перечисляет
   только изменённые ключи: `inferenceModels` и `inferenceModelPricing`.

4. После подтверждения:

   ```
   python3 .claude/skills/claude-config/generate.py --apply
   ```

5. Скажи пользователю **полностью закрыть приложение** (Quit, не просто
   закрыть окно) и открыть заново. Профиль читается при старте: правка
   файла на ходу не подхватывается, и это выглядит как «конфиг не применился».

## Правила

- **Не записывай без показа diff'а.** Скрипт делает бэкап `<profile>.bak`,
  но показать изменения всё равно обязательно.
- **Не трогай чужие ключи.** Скрипт меняет только `inferenceModels`,
  `inferenceModelPricingEnabled`, `inferenceModelPricing`. Остальное в
  профиле (адрес шлюза, токен, effort по умолчанию) остаётся как было.
- **Не редактируй профиль, пока приложение открыто**, если можно
  попросить закрыть: оно перезапишет файл при выходе.
- **Не выдумывай модели.** Источник — только `models.json`. Если модели
  нет в каталоге OpenCode Go, её нет и в пикере.

## Почему поля такие

- `name` — `picker` из `models.json`, вида `claude-GLM-5.3-Flash`. Прокси
  снимает префикс `claude-` и приводит к нижнему регистру, получая
  настоящий id модели. Префикс нужен, чтобы `claude-haiku-*` (служебные
  вызовы Claude Code) можно было отличить от явного выбора пользователя.
- `labelOverride` — человеческое имя из каталога.
- `behavesAs` — Claude-модель с тем же окном контекста. Без него
  приложение не знает лимитов чужой модели и считает окно по умолчанию.
- `maxEffort` — самый высокий уровень, который модель реально объявляет в
  каталоге. У всех он разный: `glm-5.2` знает только `high` и `max`,
  `kimi-k3` — только `max`.
- `inferenceModelPricing` — иначе приложение считает стоимость по прайсу
  Claude и завышает её в десятки раз. `cacheWritePerMtok` = input × 1.25.

## Проверка после применения

```
curl -s localhost:4000/admin/models | jq '.models | length'
```

Число должно совпасть с количеством записей в пикере.
