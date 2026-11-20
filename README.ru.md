# Regate — MCP Gateway и конструктор виртуальных инструментов

**Статус релиза.** Полный функциональный объём v1 прошёл локальную приёмку
на контролируемом стенде с независимыми MCP-провайдерами alpha и beta.
Проверенный архив и отчёт приложены к
[v1.0.0-rc.1](https://github.com/burkostya/regate/releases/tag/v1.0.0-rc.1).
Согласование безопасности и обработки персональных данных
для эксплуатации остаётся открытым в
[#113](https://github.com/burkostya/regate/issues/113).
[Статус и результаты приёмки](docs/release-status.md)

## Быстрый старт

Из корня проекта открыть два терминала.

Первый — тестовые сервисы MCP, Decision API, маскировщик PII:

```bash
go run ./cmd/regate-demo
```

Второй — Regate:

```bash
export REGATE_ADMIN_TOKEN='ЗАМЕНИТЬ_НА_НАДЕЖНЫЙ_ТОКЕН'
go run ./cmd/regate -config ./examples/regate.hcl -state ./.state
```

Вызвать виртуальный MCP-инструмент:

```bash
curl -sS http://127.0.0.1:8080/mcp/support \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"summarize_customer","arguments":{"customer_id":"C123"}}}'
```

В штатном сценарии Regate вызывает Decision API, читает CRM, отправляет результат маскировщику, а затем MCP-инструменту агента. Агент получает уже очищенный email. Попробуй передать `DENY-1` вместо `C123` — политика отклонит операцию до вызова CRM.

Получить сведения об административном MCP:

```bash
curl -sS http://127.0.0.1:8080/mcp/admin \
  -H "Authorization: Bearer $REGATE_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
```

Инструкции агентам поставляются из `skills/regate-config/SKILL.md`, `skills/regate-authoring/SKILL.md` и доступны через MCP skills API/ресурсы. Настраиваемый путь: `REGATE_SKILLS_DIR`.

## Web UI

```bash
cd web
npm install
npm run dev
```

Зайти на `http://127.0.0.1:5173` и ввести административный токен. В интерфейсе предусмотрены: upstream, surfaces, граф DAG, настройки узлов, hooks/policies, редактор HCL, черновики, ревизии, журнал вызовов и эффектов. Если gateway запущен не на `127.0.0.1:8080`, перед запуском интерфейса задать серверную переменную `REGATE_PROXY`. Она используется сервером SvelteKit в dev- и production-режимах, поэтому административный bearer не уходит в произвольные внешние URL и редиректы.

Проверить собранную консоль локально (только loopback):

```bash
cd web
npm run build
REGATE_PROXY=http://127.0.0.1:8080 HOST=127.0.0.1 PORT=5173 node build
```

**Проверка интерфейса.** Web CI запускает npm typecheck, модульные тесты,
production-сборку Node и Chromium E2E на Go-сервере с отдельным SQLite и
demo MCP. Проверяются редактирование графа и портов, CAS-конфликты, ревизии,
откат, запуск и несохранённые изменения.

## Контейнер (только для локальных экспериментов)

Есть `Dockerfile` для Go-сервера; Web UI запускается отдельно. Пример использует upstream `127.0.0.1:9091`, поэтому при запуске контейнера понадобится host network на Linux либо изменение HCL. При прослушивании всех интерфейсов обязательны `REGATE_ADMIN_TOKEN` и `REGATE_PUBLIC_TOKEN`. **Не использовать эту сборку как публичный сервис.**

## Проверки

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/regate ./cmd/regate-demo
```

Покрыты, в частности, полный цикл вызова MCP → политика → маскировка → агент, отказ Decision API, проксирование без изменения ответа, управление черновиками и откат, SSE-подписки, навыки, async task и неопределённый исход внешней записи без автоматического повторного действия.

## Основные проектные решения

- **HTTP-only**: никаких MCP stdio-процессов. Поддерживаются HTTP-запросы и потоковые SSE-ответы внутри HTTP.
- **HCL как источник правил**, отдельно JSON-представление координат графа. Конфигурация хранится в неизменяемых ревизиях, редактируется через черновики.
- **DAG Pipeline Engine**: transformation, condition, parallel/join, `mcp.call`, HTTP/MCP hooks, decision policy, mask, suspend.
- **MCP управления** (`/mcp/admin`) работает с тем же сервисом изменения конфигурации, что и REST API для Svelte.
- **Неизвестный исход побочного эффекта** — не причина слепо повторять запись. Состояние сохраняется в журнале; полноценная согласующая процедура остаётся задачей до v1.
- **Без Genkit**: агенты и LLM подключаются как внешние MCP/HTTP хуки.

## Чего не хватает до законченного v1

Полной межпортовой типизации JSON Schema, перехода входящих public/admin MCP endpoints на официальный Go SDK и полных проверок соответствия протоколу, надёжного policy/PII контроля, OAuth и ролей, полноценного reconciliation, SQLite-транзакций, корректной композиции subpipeline, уплотнения трасс и метрик OpenTelemetry, полностью протестированного Web UI и соответствующих эксплуатационных тестов. Всё перечисленное остаётся обязательным объёмом, а не «опциональным» улучшением.

**Не выставлять эту сборку в интернет и не передавать через неё реальные чувствительные данные.** Пример содержит `get_customer_direct` специально для проверки прозрачного проксирования, без PII-защиты.

Основная документация: [README.md](README.md), [архитектура](docs/architecture/overview.md), [контракт конфигурации](docs/contracts/hcl.md), [хуки](docs/contracts/hooks.md), [безопасность](docs/security.md), [состояние релиза](docs/release-status.md).
