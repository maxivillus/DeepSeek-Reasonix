# Локальные изменения Reasonix

Эта рабочая копия основана на upstream `v1.39.1` и содержит только локальные функции, нужные стеку.

## Изменения

- `read_file`: ограничение размера изображений, JPEG-сжатие и передача изображений в vision proxy.
- `memory`: автоматическое извлечение фактов, инкрементальное обновление, уровни recall, fact gate, trust и ограничение размера индекса.
- `memory-mcp`: чтение, запись и compose-путь с поддержкой workspace scope.

## Триггеры авто-экстракции

Оба триггера обязаны оставаться подключёнными — их вызовы уже терялись при ребейзе, и хост шесть недель не записывал ни одного факта:

- конец сессии: `close()` после хуков SessionEnd вызывает `memoryExtractSpawn(c.SessionPath(), c.workspaceRoot)`;
- во время сессии: `New()` запускает `startIncrementalExtraction` и сохраняет отмену в `c.incrementalStop`, `close()` её вызывает.

`memoryExtractSpawn` — намеренный шов для теста `TestCloseSpawnsMemoryExtraction`; удаление вызова делает тест красным.

## Фильтр извлечённых фактов

`junkPatterns` больше не содержит голого слова `content`: на реальном сторе оно отбрасывало 19 из 164 фактов (11.6%), задевая обычную лексику вроде `content-type` и `static content cache`. Остальные паттерны про отсутствующие сущности сохранены. Следствие: ответ-плейсхолдер вида `CONTENT` больше не отсекается фильтром.

## Отличия переноса на v1.39.1

- Общая логика сжатия изображений вынесена в `internal/imageopt`; `internal/control/imagecompress.go` оставлен как тонкая обёртка, чтобы тесты `control` не переписывать.
- `CompressForRead` никогда не отдаёт полезную нагрузку больше исходной: защита от роста больше не зависит от того, масштабировалось ли изображение. Плоские PNG-дружелюбные снимки (терминал, UI) возвращаются как есть — провайдер всё равно ограничивает разрешение на своей стороне.
- Полный автомат Goal (`goalAdvanceInput`, `advance`, `admitContinuation`, `goalContinuationSnapshot`) в upstream удалён, поэтому перенесены только `researchSkippedByFact` в `goalMachine` (`set`, `installGoalLocked`, `snapshot`, sidecar) и сам fact gate в `Controller`.
- Fact gate срабатывает и на пути session engine: у нового runtime нет класса бюджета research и он не создаёт autoresearch-задач, поэтому рычагом стал лимит раундов. Покрытая свежим фактом цель создаётся с `MaxGoalRounds = factGateRoundLimit` (3) и notice; непокрытая — с неограниченным циклом; поднять лимит можно через `/goal edit`.
- В `runOrchestratedTurn` upstream перешёл на `ComposeSynthetic` для goal-round и больше не подмешивает блок active-goal. Поэтому model-facing пометка fact gate (`factGateGoalMarker`) остаётся только на пути `compose()`/`activeGoalBlock`; на engine-пути пользователь получает notice, а не пометку в промпте.
- golden baseline `internal/boot/testdata/golden/` перегенерирован: описание `read_file` содержит локальное предложение про растры (`ToolsHash`/`PrefixHash` меняются один раз).
- `reasonix --version` показывает базу `v1.39.1` плюс локальные коммиты.

## Режимы

Основные функции memory и vision включаются настройками стека. Ограничение размера индекса применяется всегда, чтобы не превышать лимит контекста.

## Сборка

Сборка выполняется через `custom/reasonix-build.sh` из текущей чистой ветки `DeepSeek-Reasonix`. Исторические patch-файлы в `original/custom/` сохранены как источник истории и не применяются напрямую к `v1.39.1`.
