# NanoGPT Reverse Proxy — план реализации

## Цель

Красивый reverse-proxy перед `https://nano-gpt.com/api/v1`, который:

- проксирует `/chat/completions` (stream + non-stream) через единый upstream-ключ NanoGPT;
- раздаёт клиентам собственные ключи доступа и считает, кто сколько тратит;
- записывает детальный лог каждого запроса: модель, токены (input/output/reasoning/cached), цену из `x_nanogpt_pricing`, ошибки инструментов, статус, задержку;
- показывает всё это в веб-дашборде (графики, таблицы, фильтры).

Ограничения среды: VPS **512 МБ ОЗУ**, до ~10 параллельных запросов, тела до ~100 000 токенов.
Решение: **Go + SQLite + embed UI** (один бинарник, минимум памяти, нативный стриминг без буферизации тел).

---

## Архитектура

```
┌──────────┐  Bearer <client_key>   ┌──────────────────────┐  Bearer <upstream>  ┌──────────────┐
│  Client  │ ─────────────────────► │  nano-proxy (Go)     │ ──────────────────► │  nano-gpt    │
└──────────┘                        │                      │                     │   .com       │
                                    │  /v1/chat/completions│ ◄─────── SSE ────── └──────────────┘
                                    │  /admin/* (UI)       │
                                    │  /admin/api/* (JSON) │
                                    └──────────┬───────────┘
                                               │ sql.DB
                                               ▼
                                         ┌───────────┐
                                         │  SQLite   │
                                         └───────────┘
```

Один процесс, два набора маршрутов:
- `/v1/*` — публичный прокси-протокол (OpenAI-совместимый, NanoGPT-совместимый).
- `/admin/*` — дашборд и admin-API (закрыт отдельным `ADMIN_TOKEN`).

---

## Стек

| Слой | Выбор | Почему |
|---|---|---|
| Язык | Go 1.22+ | Низкое потребление памяти (~20–40 МБ RSS), быстрый стриминг, один статический бинарь |
| HTTP | `net/http` + chi (роутер) | Достаточно, без лишнего веса |
| Upstream | `net/http` клиент с `Flusher` | Стримим чанки `io.Copy` без аллокаций |
| DB | `modernc.org/sqlite` (pure Go) | Без cgo, простая компиляция, WAL-режим |
| Шаблоны UI | `html/template` + `embed.FS` | Один бинарь, без CDN |
| CSS | минимальный hand-written (тёмная тема, glass cards, акценты) | "Красивый" по дефолту без сборщиков |
| Charts | `uPlot` (≈40 КБ, встраиваем локально) | Лёгкий, без зависимостей |
| Парсинг JSON | `encoding/json` + `json.Decoder` для потока | SSE-чанки парсятся инкрементально |
| Auth | HMAC-SHA256 для `client_key` (хеш в БД) | Без хранения ключей в открытом виде |
| Конфиг | YAML (`config.yaml`) + ENV override | Просто и читаемо |

---

## Структура проекта

```
revers-proxy-api/
├── cmd/
│   └── nano-proxy/
│       └── main.go              # точка входа, загрузка конфига, запуск сервера
├── internal/
│   ├── config/                  # парсинг config.yaml
│   ├── auth/                    # HMAC-verify клиентских ключей, admin-token
│   ├── proxy/
│   │   ├── proxy.go             # основной прокси /v1/chat/completions
│   │   ├── stream.go            # парсинг SSE, извлечение usage + x_nanogpt_pricing
│   │   └── tools.go             # детекция tool_calls и ошибок в них
│   ├── store/
│   │   ├── sqlite.go            # подключение, миграции
│   │   ├── keys.go              # CRUD клиентских ключей
│   │   ├── requests.go          # запись лога запросов
│   │   └── analytics.go         # агрегаты для дашборда
│   ├── handlers/
│   │   ├── admin_api.go         # JSON API для UI
│   │   └── admin_ui.go          # HTML-страницы дашборда
│   └── ui/
│       ├── embed.go             # //go:embed web/
│       └── templates/           # html/template
├── web/                         # статика и шаблоны (встраивается в бинарь)
│   ├── templates/
│   │   ├── layout.html
│   │   ├── dashboard.html
│   │   ├── requests.html
│   │   └── keys.html
│   └── static/
│       ├── app.css
│       ├── app.js
│       └── uplot.min.js
├── config.example.yaml
├── go.mod
├── go.sum
├── PLAN.md
└── README.md
```

---

## Модель данных (SQLite, WAL)

```sql
-- Клиентские ключи доступа к нашему прокси
CREATE TABLE api_keys (
  id            INTEGER PRIMARY KEY,
  name          TEXT    NOT NULL,          -- "alice-app", "bot-tg-1"
  key_prefix    TEXT    NOT NULL,          -- первые 8 символов для UI ("sknp_AbCd…")
  key_hash      TEXT    NOT NULL UNIQUE,   -- HMAC-SHA256(ADMIN_SECRET, raw_key)
  enabled       INTEGER NOT NULL DEFAULT 1,
  budget_usd    REAL    NULL,              -- опциональный мягкий лимит
  created_at    INTEGER NOT NULL
);

-- Одна строка = один запрос клиента (даже стрим — это один "request")
CREATE TABLE requests (
  id              INTEGER PRIMARY KEY,
  ts              INTEGER NOT NULL,         -- unix ms, время приёма
  finished_ts     INTEGER,                  -- время завершения
  api_key_id      INTEGER NOT NULL REFERENCES api_keys(id),
  model           TEXT    NOT NULL,
  stream          INTEGER NOT NULL,
  status_code     INTEGER NOT NULL,         -- финальный HTTP код
  error_type      TEXT,                      -- "upstream_4xx", "upstream_5xx", "client_abort", "tool_error", "parse_error"
  error_message   TEXT,
  prompt_tokens        INTEGER DEFAULT 0,
  completion_tokens    INTEGER DEFAULT 0,
  reasoning_tokens     INTEGER DEFAULT 0,
  cached_tokens        INTEGER DEFAULT 0,
  cache_creation_tokens INTEGER DEFAULT 0,
  cache_read_tokens    INTEGER DEFAULT 0,
  total_tokens         INTEGER DEFAULT 0,
  cost_usd        REAL DEFAULT 0,           -- из x_nanogpt_pricing.cost
  payment_source  TEXT,                      -- из x_nanogpt_pricing.paymentSource
  latency_ms      INTEGER,                  -- upstream latency
  has_tool_calls  INTEGER DEFAULT 0,
  tool_calls_count INTEGER DEFAULT 0,
  tool_error      INTEGER DEFAULT 0,
  tool_error_msg  TEXT,
  upstream_request_id TEXT,
  client_ip       TEXT,
  user_agent      TEXT
);

CREATE INDEX idx_requests_ts       ON requests(ts DESC);
CREATE INDEX idx_requests_key_ts   ON requests(api_key_id, ts DESC);
CREATE INDEX idx_requests_model_ts ON requests(model, ts DESC);
CREATE INDEX idx_requests_status   ON requests(status_code);

-- Дневные агрегаты (накапливаются в коде для скорости чтения дашборда)
CREATE TABLE daily_stats (
  day           TEXT    NOT NULL,           -- "YYYY-MM-DD"
  api_key_id    INTEGER NOT NULL,
  model         TEXT    NOT NULL,
  requests      INTEGER DEFAULT 0,
  errors        INTEGER DEFAULT 0,
  input_tokens  INTEGER DEFAULT 0,
  output_tokens INTEGER DEFAULT 0,
  cached_tokens INTEGER DEFAULT 0,
  cost_usd      REAL    DEFAULT 0,
  PRIMARY KEY (day, api_key_id, model)
);

CREATE TABLE daily_key_totals (
  day        TEXT NOT NULL,
  api_key_id INTEGER NOT NULL,
  requests   INTEGER DEFAULT 0,
  errors     INTEGER DEFAULT 0,
  cost_usd   REAL DEFAULT 0,
  tokens     INTEGER DEFAULT 0,
  PRIMARY KEY (day, api_key_id)
);
```

Запись идёт сразу в `requests` + инкрементально в `daily_*` через одну транзакцию.

---

## Поток проксирования

### Non-stream
1. Принять `POST /v1/chat/completions`, прочитать тело (≤1 МБ — буферизуем, это маленькие).
2. Авторизовать клиента по `Authorization: Bearer <raw>`, найти `api_key_id`.
3. Запомнить `t0`, выставить upstream-заголовки (`Authorization: Bearer ${NANOGPT_API_KEY}`), проксировать на `https://nano-gpt.com/api/v1/chat/completions`.
4. Получить ответ, распарсить `usage` + `x_nanogpt_pricing` + `choices[*].message.tool_calls` → записать строку в `requests` + агрегаты.
5. Вернуть ответ клиенту как есть.

### Stream (SSE)
1. Заголовок `Accept: text/event-stream` — проксируем с `Transfer-Encoding: chunked`, **без буферизации**.
2. Запускаем `io.Copy` от upstream к клиенту через `http.Flusher`.
3. Параллельно парсим каждую SSE-строку `data: {...}` через `json.Decoder`:
   - Накапливаем только `usage` и `x_nanogpt_pricing` из **последнего** чанка перед `[DONE]`.
   - Детектим `choices[*].delta.tool_calls` — инкрементим `tool_calls_count`.
   - Детектим `error` поле (некоторые провайдеры шлют ошибку в стриме) — фиксируем `error_type`.
4. На `data: [DONE]` или обрыве — пишем одну строку в `requests` + агрегаты **асинхронно** (в горутине, не блокируя отдачу чанков клиенту).
5. Закрываем upstream-коннект.

### Память при стриме
Держим только:
- `lastUsage` — один объект `Usage{}` (несколько байт).
- `accumTools` — счётчик tool_calls и список их имён (несколько сотен байт).
- Никакого `strings.Builder` для контента — нам не нужен полный текст ответа для аналитики.

→ На 10 параллельных стримах по 100k токенов пик аллокаций нашего процесса остаётся в пределах 30–50 МБ.

---

## Tool errors

Извлекаются из ответа:
- В non-stream: парсим `choices[*].message.tool_calls[*]`, ищем поле `error` / `status="error"` / `code != null` внутри аргументов или рядом с tool_call.
- В stream: то же самое в `delta.tool_calls[*]`.
- Если у финального чанка есть `usage`, но `finish_reason != "tool_calls"` при наличии tool_calls → считаем как `tool_error=1`.
- Если парсинг `arguments` упал (невалидный JSON) → `tool_error=1, tool_error_msg="invalid_arguments_json"`.

---

## Цены

Не ведём свою таблицу — берём `x_nanogpt_pricing.cost` и `paymentSource` прямо из ответа NanoGPT (видно в SSE-чанке выше).
- Для non-stream: парсим верхний уровень.
- Для stream: берём из последнего чанка перед `[DONE]`.
- Если поля нет — `cost_usd=0` + `error_type="missing_pricing_info"` (не блокируем).

Это покрывает требование пользователя и работает для любой модели без обновлений.

---

## Эндпоинты

### Прокси (публичные)

| Метод | Путь | Назначение |
|---|---|---|
| `POST` | `/v1/chat/completions` | Проксирование с записью метрик |
| `GET`  | `/v1/models` | Проксирование (без метрик, только список моделей) |
| `GET`  | `/healthz` | `200 OK` для проверки живости |

> В рамках MVP покрываем только `/chat/completions`. Остальные NanoGPT-эндпоинты (`/responses`, `/messages`, images) — следующий этап.

### Admin UI (HTML)

| Путь | Страница |
|---|---|
| `GET /admin/` | Главная: KPI-карточки + графики за сегодня/7д/30д |
| `GET /admin/requests` | Таблица всех запросов с фильтрами (по ключу, модели, статусу, диапазону) |
| `GET /admin/requests/{id}` | Детальный просмотр одного запроса (заголовки, body-summary, usage, цена, tool_calls) |
| `GET /admin/keys` | Список клиентских ключей |
| `GET /admin/login` | Форма входа по `ADMIN_TOKEN` (HMAC-cookie) |

### Admin API (JSON, для UI и внешних интеграций)

| Метод | Путь | Назначение |
|---|---|---|
| `POST` | `/admin/api/login` | Авторизация админа → cookie |
| `POST` | `/admin/api/logout` | Выход |
| `GET`  | `/admin/api/keys` | Список ключей |
| `POST` | `/admin/api/keys` | Создать ключ (генерирует `raw_key`, показывает один раз) |
| `PATCH`| `/admin/api/keys/{id}` | Включить/выключить, переименовать, задать бюджет |
| `DELETE`| `/admin/api/keys/{id}` | Удалить |
| `GET`  | `/admin/api/stats/summary?range=7d` | KPI: total/avg/по дням |
| `GET`  | `/admin/api/stats/timeseries?range=7d&key=&model=` | uPlot-серии: requests, tokens, cost |
| `GET`  | `/admin/api/stats/breakdown?by=key\|model` | Топ-N по тратам/токенам/ошибкам |
| `GET`  | `/admin/api/requests?key=&model=&status=&from=&to=&limit=&offset=` | Список с фильтрами |
| `GET`  | `/admin/api/requests/{id}` | Детальный JSON |

---

## Дизайн дашборда ("красивый")

Одна тёмная палитра в стиле Linear/Vercel-стиля, всё inline в embed-UI:

**Главная (`/admin/`)**
- Шапка: лого "nano-proxy" + диапазон (24h / 7d / 30d / custom) + текущий аптайм.
- 4 KPI-карточки (glass-effect): **Всего $**, **Запросов**, **Токенов (in/out)**, **Cache hit rate %**.
- 2 больших графика (uPlot): **$ / день** и **Tokens / день** (stacked: input/output/cached).
- Слева таблица "Топ-5 ключей по расходам", справа "Топ-5 моделей".
- Внизу — последние 10 запросов (статус, модель, токены, $, ключ).

**Запросы (`/admin/requests`)**
- Фильтры в шапке: ключ (select), модель (select), статус (chips), диапазон дат, кнопка "Export CSV".
- Таблица с виртуализацией (или просто пагинация 50/стр.).
- Бейджи статусов: 200 зелёный, 4xx оранжевый, 5xx красный, tool_error жёлтый.
- Колонка "Cache hit" показывает `cached_tokens / prompt_tokens` цветом.

**Детальный запрос**
- Левый блок: заголовки, тело (system+user truncated, последние 3 message), tool_calls список.
- Правый блок: метрики (usage, cost, latency, finish_reason), ссылки на tool_calls и возможные ошибки.
- Если есть tool_error — красная плашка с диагностикой.

**Ключи (`/admin/keys`)**
- Таблица: имя, префикс ключа (raw никогда не показывается), статус, расход за 30д, бюджет, кнопки.
- Модалка создания: имя + опциональный бюджет → создать → одноразово показать полный ключ.

Стиль: шрифт Inter, фон `#0b0d10`, карточки `rgba(255,255,255,0.04)` с `backdrop-filter: blur(12px)`, акцент `#7c5cff` (фиолетовый), графики в той же гамме.

---

## Конфигурация (`config.yaml`)

```yaml
server:
  listen: "0.0.0.0:8080"
  read_timeout: 60s
  write_timeout: 0      # 0 = без таймаута для стримов
  idle_timeout: 120s

upstream:
  base_url: "https://nano-gpt.com"
  api_key: "${NANOGPT_API_KEY}"   # подставляется из ENV
  path_prefix: "/api/v1"

storage:
  db_path: "./data/nano-proxy.db"
  retention_days: 90              # периодическая чистка старых запросов

admin:
  token: "${ADMIN_TOKEN}"
  cookie_secret: "${ADMIN_COOKIE_SECRET}"

limits:
  max_body_bytes: 2097152         # 2 МБ на тело запроса (защита)
  max_concurrent: 64              # семафор на апстрим
  request_timeout: 300s           # общий таймаут upstream
```

---

## Метрики для дашборда (агрегаты)

- **Суммы**: requests, errors (4xx+5xx+tool_error), input/output/cached tokens, cost_usd.
- **Производные**: cache hit rate = `Σcached / Σprompt_tokens`, $/1k tokens, avg latency, error rate.
- **Разрезы**: by_day, by_key, by_model, by_(day,key), by_(day,model).
- **Топы**: top-N ключей по `cost_usd` за период; top-N моделей по запросам и по цене.

Считается на лету через SQL с `GROUP BY` (для 90 дней и 10 rps это <50мс на SQLite с индексом).

---

## Безопасность

- Клиентские ключи: генерируем `sknp_` + 40 hex, храним только HMAC-SHA256(cookie_secret, raw).
- Admin-вход по `ADMIN_TOKEN` (HMAC-cookie, httpOnly, SameSite=Strict, 12 часов).
- Bind по умолчанию `127.0.0.1` (админка), прокси слушает на всех интерфейсах через отдельный listener.
- Опциональный basic-auth на `/admin` через `ADMIN_BASIC_AUTH`.
- Лимит `max_body_bytes` от клиента + `Content-Length` чек.
- Семафор `max_concurrent` на upstream (защита NanoGPT-ключа от перегруза).
- Не логируем raw request/response body — только структурные поля.

---

## Деплой на VPS 512 МБ

- Сборка: `go build -ldflags="-s -w" -o nano-proxy ./cmd/nano-proxy` → один бинарь ~10–12 МБ.
- Systemd unit (`/etc/systemd/system/nano-proxy.service`):
  ```
  EnvironmentFile=/etc/nano-proxy/env
  ExecStart=/opt/nano-proxy/nano-proxy -config /etc/nano-proxy/config.yaml
  Restart=on-failure
  ```
- Параметры: `GOMEMLIMIT=400MiB`, `GOGC=50` (сборщик мусора чаще, но меньше паузы).
- Перед запуском: `ulimit -n 65535` для лимита FD (важно при стримах).
- Опционально: `caddy`/`nginx` перед прокси для TLS.

---

## План реализации (порядок шагов)

1. **Scaffold** — `go mod init`, chi, modernc-sqlite, embed.FS, config-парсер, hello-world сервер.
2. **Хранилище** — миграции, типы `Store`, ключи, запись запросов, daily_stats.
3. **Прокси non-stream** — базовый `/v1/chat/completions` с auth-мидлварой и записью метрик.
4. **Прокси stream** — SSE-парсер, извлечение usage/x_nanogpt_pricing, tool_calls.
5. **Tool errors** — детекция ошибок инструментов в stream и non-stream.
6. **Admin auth** — login/logout, cookie, middleware для `/admin/*`.
7. **Admin UI shell** — layout, статика, embed, тёмная тема.
8. **Admin API: keys CRUD** — создать/показать один раз/включить/удалить.
9. **Admin API: stats + requests list/detail** — агрегаты, фильтры, CSV export.
10. **Дашборд-страницы** — KPI, графики, таблица запросов, ключи.
11. **uPlot интеграция** — серии, тултипы, темизация.
12. **Polish** — пагинация, виртуализация таблицы, форма диапазона дат, экспорт.
13. **Systemd + README** — инструкция деплоя на 512 МБ VPS.
14. **Smoke-тесты** — скрипт `scripts/smoke.sh` с 3 примерами: non-stream, stream, с tools.

---

## Что НЕ делаем в MVP

- BYOK, x402, провайдер-роутинг — не нужно пользователю.
- Image/video/audio endpoints — следующая итерация.
- Аутентификация по OAuth/SSO — хватает `ADMIN_TOKEN`.
- Графана/Прометей — собственный дашборд покрывает требования.
- Распределённый режим — один инстанс.

---

## Критерии готовности MVP

- [ ] Запускается одним бинарём, конфиг в YAML, ENV для секретов.
- [ ] Принимает `POST /v1/chat/completions` от клиента с `Bearer <client_key>`.
- [ ] Возвращает ответ NanoGPT как есть (stream и non-stream, байт-в-байт).
- [ ] Для каждого запроса пишет строку в `requests` со всеми метриками.
- [ ] Извлекает цену из `x_nanogpt_pricing.cost`.
- [ ] Детектит tool_calls и tool_errors.
- [ ] Дашборд показывает: KPI, графики по дням, топ ключей/моделей, таблицу запросов, CRUD ключей.
- [ ] На тесте 10 параллельных стримов по 100к токенов RSS ≤ 80 МБ.
- [ ] Деплой-инструкция для 512 МБ VPS (systemd unit, GOMEMLIMIT).