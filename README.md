# report-service

Сервис PDF-отчётов Inspecto (Go): задания на отчёт об осмотре, генерация через Gotenberg, хранение и выдача PDF (ADR-002, ADR-011, ADR-012, ADR-026).
Документация проекта, контракт API (`api/report.yaml`) и окружение разработки — в репозитории
[platform](https://github.com/inspecto-system/platform).

Сейчас это каркас (T-401): конфигурация, база данных с миграциями, HTTP-сервер с `/health/*`, логи и проверка JWT. Задания, генерация PDF и API отчётов — T-402.

## Структура

| Путь | Назначение |
|---|---|
| `cmd/report-service` | Точка входа: конфигурация, сигналы остановки |
| `internal/app` | Сборка сервиса: миграции, HTTP-сервер, плавная остановка |
| `internal/auth` | Проверка JWT пользователей и сервисных токенов по JWKS core-service (только RS256, `iss`, `aud`, `exp`) |
| `internal/config` | Настройки из переменных окружения |
| `internal/httpapi` | Маршрутизация, `X-Request-Id`, журнал запросов, ошибки RFC 9457, `/health/live` и `/health/ready` |
| `internal/logging` | JSON-логи с полями ECS, как у core-service и media-service |
| `internal/storage` | PostgreSQL (pgx), миграции (встроены в бинарник) |
| `internal/testsupport` | PostgreSQL в Testcontainers, выпуск тестовых JWT |

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

Пароли обязательны и передаются только через окружение (SECURITY.md §5); сервис сообщает обо всех отсутствующих сразу.

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
