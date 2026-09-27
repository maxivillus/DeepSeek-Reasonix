# Локальные изменения Reasonix

Эта рабочая копия основана на upstream `v1.39.1` и содержит только локальные функции, нужные стеку.

## Изменения

- `read_file`: ограничение размера изображений, JPEG-сжатие и передача изображений в vision proxy.
- `memory`: автоматическое извлечение фактов, инкрементальное обновление, уровни recall, fact gate, trust и ограничение размера индекса.
- `memory-mcp`: чтение, запись и compose-путь с поддержкой workspace scope.

## Отличия переноса на v1.39.1

- Общая логика сжатия изображений вынесена в `internal/imageopt`; `internal/control/imagecompress.go` оставлен как тонкая обёртка, чтобы тесты `control` не переписывать.
- Полный автомат Goal (`goalAdvanceInput`, `advance`, `admitContinuation`, `goalContinuationSnapshot`) в upstream удалён, поэтому перенесены только `researchSkippedByFact` в `goalMachine` (`set`, `installGoalLocked`, `snapshot`, sidecar) и сам fact gate в `Controller`.
- В `runOrchestratedTurn` upstream перешёл на `ComposeSynthetic` для goal-round и больше не подмешивает блок active-goal. Поэтому model-facing пометка fact gate (`factGateGoalMarker`) остаётся только на пути `compose()`/`activeGoalBlock`; notice пользователю сохраняется в `Controller`.
- `reasonix --version` показывает базу `v1.39.1` плюс локальный коммит.

## Режимы

Основные функции memory и vision включаются настройками стека. Ограничение размера индекса применяется всегда, чтобы не превышать лимит контекста.

## Сборка

Сборка выполняется через `custom/reasonix-build.sh` из текущей чистой ветки `DeepSeek-Reasonix`. Исторические patch-файлы в `original/custom/` сохранены как источник истории и не применяются напрямую к `v1.39.1`.
