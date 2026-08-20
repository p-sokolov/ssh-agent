package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"golang.org/x/crypto/ssh"
)

//go:embed templates/*.sh
var templatesFS embed.FS

var db *sql.DB

type CreateRequest struct {
	Host     string `json:"host"`
	User     string `json:"user"`
	Password string `json:"password"`
	Template string `json:"template"`
}

type CallbackPayload struct {
	User   string `json:"user"`
	Script string `json:"script"`
	Action string `json:"action"`
	Time   string `json:"time"`
}

func main() {
	initDB()

	mux8080 := http.NewServeMux()
	mux8080.HandleFunc("POST /create", handleCreate)

	mux8081 := http.NewServeMux()
	mux8081.HandleFunc("POST /callback", handleCallback)

	srv1 := &http.Server{Addr: ":8080", Handler: mux8080}
	srv2 := &http.Server{Addr: ":8081", Handler: mux8081}

	go func() {
		slog.Info("API 1 started on :8080")
		if err := srv1.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Server 8080 failed", "err", err)
		}
	}()

	go func() {
		slog.Info("API 2 started on :8081")
		if err := srv2.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Server 8081 failed", "err", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = srv1.Shutdown(shutdownCtx)
	_ = srv2.Shutdown(shutdownCtx)
	if db != nil {
		db.Close()
	}
	slog.Info("Servers stopped cleanly")
}

func initDB() {
	dsn := os.Getenv("DB_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/secops?sslmode=disable"
	}

	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		slog.Warn("DB connection failed, skipping DB persistence", "err", err)
		return
	}

	schema := `
	CREATE TABLE IF NOT EXISTS deployments (
		id SERIAL PRIMARY KEY,
		host VARCHAR(255) NOT NULL,
		user_name VARCHAR(255) NOT NULL,
		script_path VARCHAR(255) NOT NULL,
		created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS events (
		id SERIAL PRIMARY KEY,
		user_name VARCHAR(255) NOT NULL,
		script_path VARCHAR(255) NOT NULL,
		action VARCHAR(50) NOT NULL,
		event_time TIMESTAMP WITH TIME ZONE NOT NULL,
		created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
	);
	`
	if _, err := db.Exec(schema); err != nil {
		slog.Warn("Could not init DB tables", "err", err)
	} else {
		slog.Info("DB connected and tables verified")
	}
}

func handleCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}

	content, err := templatesFS.ReadFile(fmt.Sprintf("templates/%s.sh", req.Template))
	if err != nil {
		http.Error(w, "template not found", http.StatusBadRequest)
		return
	}

	cleanContent := strings.ReplaceAll(string(content), "\r\n", "\n")

	scriptPath := fmt.Sprintf("/tmp/%s.sh", req.Template)
	if err := deployToRemoteHost(req, scriptPath, cleanContent); err != nil {
		slog.Error("Deploy failed", "host", req.Host, "err", err)
		http.Error(w, fmt.Sprintf("deploy failed: %v", err), http.StatusInternalServerError)
		return
	}

	if db != nil {
		_, _ = db.Exec("INSERT INTO deployments (host, user_name, script_path) VALUES ($1, $2, $3)",
			req.Host, req.User, scriptPath)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"created and monitoring started"}`))
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	var cb CallbackPayload
	if err := json.NewDecoder(r.Body).Decode(&cb); err != nil {
		http.Error(w, "invalid callback payload", http.StatusBadRequest)
		return
	}

	fmt.Printf("[%s]\nAction: %s\nScript: %s\nTime: %s\n",
		cb.User, cb.Action, cb.Script, cb.Time)

	if db != nil {
		parsedTime, _ := time.Parse(time.RFC3339, cb.Time)
		if parsedTime.IsZero() {
			parsedTime = time.Now()
		}
		_, _ = db.Exec("INSERT INTO events (user_name, script_path, action, event_time) VALUES ($1, $2, $3, $4)",
			cb.User, cb.Script, cb.Action, parsedTime)
	}

	w.WriteHeader(http.StatusOK)
}

func deployToRemoteHost(req CreateRequest, targetScript, scriptContent string) error {
	config := &ssh.ClientConfig{
		User:            req.User,
		Auth:            []ssh.AuthMethod{ssh.Password(req.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	addr := req.Host
	if !hasPort(addr) {
		addr = addr + ":22"
	}

	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()

	agentScript := "/tmp/agent_monitor.sh"

	// Запись целевого скрипта
	writeCmd := fmt.Sprintf("cat << 'EOF' > %s\n%s\nEOF\nchmod +x %s", targetScript, scriptContent, targetScript)
	if err := runCommand(client, writeCmd); err != nil {
		return fmt.Errorf("write target script: %w", err)
	}

	// Скрипт фонового агента
	agentBody := fmt.Sprintf(`#!/bin/bash
TARGET="%s"
CALLBACK_URL="http://127.0.0.1:8081/callback"
USER_NAME="$(whoami)"

if which inotifywait >/dev/null 2>&1; then
  inotifywait -m -e open,access "$TARGET" --format '%%e' 2>/dev/null | while read event; do
    ACTION="open"
    if pgrep -f "$TARGET" >/dev/null 2>&1; then
      ACTION="execute"
    fi
    curl -s -X POST "$CALLBACK_URL" \
      -H "Content-Type: application/json" \
      -d "{\"user\":\"$USER_NAME\",\"script\":\"$TARGET\",\"action\":\"$ACTION\",\"time\":\"$(date -u +"%%Y-%%m-%%dT%%H:%%M:%%SZ")\"}"
  done
fi
`, targetScript)

	writeAgentCmd := fmt.Sprintf("cat << 'EOF' > %s\n%s\nEOF\nchmod +x %s", agentScript, agentBody, agentScript)
	if err := runCommand(client, writeAgentCmd); err != nil {
		return fmt.Errorf("write agent script: %w", err)
	}

	startAgentCmd := fmt.Sprintf("nohup %s > /dev/null 2>&1 &", agentScript)
	return runCommand(client, startAgentCmd)
}

func runCommand(client *ssh.Client, cmd string) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	return session.Run(cmd)
}

func hasPort(s string) bool {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return true
		}
	}
	return false
}