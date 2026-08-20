# SSH Remote Script Deployment & Monitoring Service

Сервис на Go для удаленного подключения по SSH, создания bash-скриптов по шаблонам, установки агента мониторинга (`inotifywait`) и сбора событий (`open`/`execute`).

## Архитектура и функционал
- **API №1 (`:8080`)**: Обрабатывает `POST /create` — подключается по SSH к целевой машине, деплоит bash-скрипт и запускает фоновый агент отслеживания.
- **API №2 (`:8081`)**: Обрабатывает `POST /callback` — принимает события активности над скриптом от агента, логирует их в консоль и сохраняет в БД.
- **Шаблоны**: Встроены в бинарник через `//go:embed` (`template1`, `template2`).
- **База данных**: PostgreSQL (таблицы `deployments` и `events`).
- **Graceful Shutdown**: Безопасная остановка обоих серверов по сигналам ОС (`SIGINT`, `SIGTERM`).

## Запуск

### 1. Запуск базы данных
```bash
docker run -d --name test-postgres -p 5432:5432 -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=secops postgres:15-alpine
```

### 2. Запуск сервиса
```bash
go run main.go
```

## Тестирование

### Шаг 1. Создание скрипта на удаленном хосте (API 1)
```bash
curl -X POST http://localhost:8080/create \
  -H "Content-Type: application/json" \
  -d '{
    "host": "127.0.0.1:22",
    "user": "your_user_name",
    "password": "your_password",
    "template": "template1"
  }'
```

### Шаг 2. Генерация событий на целевой машине
- **Открытие файла (`open`)**:
  ```bash
  cat /tmp/template1.sh
  ```
- **Выполнение файла (`execute`)**:
  ```bash
  /tmp/template1.sh
  ```

### Шаг 3. Проверка событий в БД
```bash
docker exec -it test-postgres psql -U postgres -d secops -c "SELECT * FROM deployments;"
docker exec -it test-postgres psql -U postgres -d secops -c "SELECT * FROM events;"
```