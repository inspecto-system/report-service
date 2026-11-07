# report-service

Сервис PDF-отчётов Inspecto (Go): задания на отчёт об осмотре, генерация через Gotenberg, хранение и выдача PDF (ADR-002, ADR-011, ADR-012, ADR-026).
Документация проекта, контракт API (`api/report.yaml`) и окружение разработки — в репозитории
[platform](https://github.com/inspecto-system/platform).

Как работает: `POST /api/v1/reports` проверяет через core-service доступ к осмотру и то, что он завершён, и ставит задание в очередь (таблица `reports`). Обработчики забирают задания (`FOR UPDATE SKIP LOCKED`), берут данные отчёта из core (`/internal/v1/inspections/{id}/report-data`) и превью фото дефектов из media-service, собирают HTML (шрифты PT Sans / PT Serif встроены, без внешних ссылок), Gotenberg превращает его в PDF, файл кладётся в бакет `reports`. Временные сбои повторяются (до `REPORT_MAX_ATTEMPTS`), зависшие задания возвращаются в очередь. `DELETE /internal/v1/reports` (scope `reports.manage`) удаляет отчёты объекта или пользователя.

## Структура

| Путь | Назначение |
|---|---|
| `cmd/report-service` | Точка входа: конфигурация, сигналы остановки |
| `internal/app` | Сборка сервиса: миграции, HTTP-сервер, обработчики заданий, плавная остановка |
| `internal/auth` | Проверка JWT пользователей и сервисных токенов по JWKS core-service (только RS256, `iss`, `aud`, `exp`) |
| `internal/config` | Настройки из переменных окружения |
| `internal/core` | Клиент core-service: сервисный токен, проверка доступа, данные отчёта |
| `internal/media` | Клиент media-service: превью фото |
| `internal/httpapi` | Маршрутизация, `X-Request-Id`, журнал запросов, ошибки RFC 9457, `/health/*`, API отчётов |
| `internal/jobs` | Очередь генерации: обработчики, повторы, возврат зависших заданий |
| `internal/logging` | JSON-логи с полями ECS, как у core-service и media-service |
| `internal/objectstore` | Хранилище PDF (S3 API, SeaweedFS) |
| `internal/render` | HTML-шаблон отчёта, подписи ru/en, шрифты, клиент Gotenberg |
| `internal/service` | Правила: доступ, только завершённый осмотр, `Idempotency-Key`, удаление файлов раньше строк |
| `internal/storage` | PostgreSQL (pgx), миграции (встроены в бинарник), таблица заданий |
| `internal/testsupport` | PostgreSQL, SeaweedFS и Gotenberg в Testcontainers, выпуск тестовых JWT |

## Настройки

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `REPORT_PORT` | `8083` | HTTP-порт |
| `REPORT_DB_URL` | `postgres://127.0.0.1:5432/inspecto_report?sslmode=require` | База без логина и пароля |
| `REPORT_DB_APP_USER` / `REPORT_DB_APP_PASSWORD` | `report_app` / — | Роль приложения (только DML) |
| `REPORT_DB_MIGRATOR_USER` / `REPORT_DB_MIGRATOR_PASSWORD` | `report_migrator` / — | Владелец схемы, только для миграций |
| `REPORT_JWKS_URL` | `http://127.0.0.1:8081/api/v1/.well-known/jwks.json` | Открытые ключи core |
| `REPORT_JWT_ISSUER` / `REPORT_JWT_AUDIENCE` | `inspecto-core` / `inspecto-api` | Ожидаемые `iss` и `aud` токена |
| `REPORT_JWT_INTERNAL_AUDIENCE` | `inspecto-internal` | `aud` сервисных токенов, принимаются только на `/internal/**` |
| `REPORT_CORE_URL` / `REPORT_CORE_CLIENT_SECRET` | `http://127.0.0.1:8081` / — | core-service и секрет клиента `report-service` |
| `REPORT_MEDIA_URL` | `http://127.0.0.1:8082` | media-service |
| `REPORT_GOTENBERG_URL` | `http://127.0.0.1:3000` | Gotenberg |
| `REPORT_S3_ENDPOINT` / `REPORT_S3_REGION` / `REPORT_S3_BUCKET` | `http://127.0.0.1:8333` / `us-east-1` / `reports` | Хранилище PDF |
| `REPORT_S3_ACCESS_KEY` / `REPORT_S3_SECRET_KEY` | — | Ключи хранилища |
| `REPORT_MAX_SIZE_BYTES` | `20971520` (20 МБ) | Предел размера PDF, больше — `REPORT_TOO_LARGE` |
| `REPORT_WORKERS` | `2` | Параллельные обработчики |
| `REPORT_MAX_ATTEMPTS` | `3` | Попыток на задание при временных сбоях |
| `REPORT_RENDER_TIMEOUT` | `2m` | Время на одну генерацию |
| `REPORT_TIME_ZONE` | `Europe/Moscow` | Часовой пояс дат в отчёте |

Пароли, секрет и ключи обязательны и передаются только через окружение (SECURITY.md §5); сервис сообщает обо всех отсутствующих сразу.

Шрифты PT Sans и PT Serif (ParaType) распространяются по лицензии SIL Open Font License 1.1, тексты лицензий — в `internal/render/fonts`.

## Проверки

Go на компьютере не нужен: инструменты запускаются в закреплённых Docker-образах. С `make` (Linux, WSL):

```bash
make check   # golangci-lint, сборка, тесты с -race и порогом покрытия, govulncheck
```

Без `make` (Git Bash в Windows) — те же команды напрямую:

```bash
export MSYS_NO_PATHCONV=1
docker run --rm -v "$(pwd -W):/src" -v inspecto-gomod:/go/pkg/mod -w /src golangci/golangci-lint:v2.15.0 golangci-lint run ./...
docker run --rm -v "$(pwd -W):/src" -v inspecto-gomod:/go/pkg/mod -v /var/run/docker.sock:/var/run/docker.sock -e TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal -e TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock -w /src golang:1.27.3 go test -race -cover ./...
```

## Запуск против dev-окружения

PostgreSQL из `platform/deploy` (пароли — из `deploy/.env`):

```bash
docker compose -f ../platform/deploy/compose.dev.yaml --env-file ../platform/deploy/.env up -d --wait postgres
set -a; . ../platform/deploy/.env; set +a
docker run --rm -p 127.0.0.1:8083:8083 -v "$(pwd -W):/src" -v inspecto-gomod:/go/pkg/mod -w /src \
  -e REPORT_DB_URL="postgres://host.docker.internal:${POSTGRES_PORT}/inspecto_report?sslmode=require" \
  -e REPORT_DB_APP_PASSWORD="$REPORT_APP_PASSWORD" -e REPORT_DB_MIGRATOR_PASSWORD="$REPORT_MIGRATOR_PASSWORD" \
  golang:1.27.3 go run ./cmd/report-service
```

Для генерации отчётов сервису также нужны core, media, SeaweedFS и Gotenberg: передайте `REPORT_CORE_CLIENT_SECRET`, `REPORT_S3_ACCESS_KEY`, `REPORT_S3_SECRET_KEY` и адреса зависимостей через `-e` так же, как пароли базы.
