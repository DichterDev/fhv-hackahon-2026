package main

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

//go:embed init.sql
var initSQL string

type Config struct {
	Port        string
	DBPath      string
	OllamaURL   string
	OllamaModel string
	NumPredict  int
	TDURL       string
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func loadConfig() Config {
	llmHost := getEnv("HACK_LLM", "100.64.0.4")
	llmPort := getEnv("HACK_LLM_PORT", "11434")
	tdHost := getEnv("HACK_TD", "100.64.0.5")
	tdPort := getEnv("HACK_TD_PORT", "9000")

	return Config{
		Port:        getEnv("PORT", "8080"),
		DBPath:      getEnv("DB_PATH", "/data/hackathon.db"),
		OllamaURL:   fmt.Sprintf("http://%s:%s/api/generate", llmHost, llmPort),
		OllamaModel: getEnv("OLLAMA_MODEL", "qwen3:8b"),
		NumPredict:  getEnvInt("OLLAMA_NUM_PREDICT", 150),
		TDURL:       fmt.Sprintf("http://%s:%s/api/tokens", tdHost, tdPort),
	}
}

type PromptRecord struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Prompt    string `json:"prompt"`
	Response  string `json:"response"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	Status    string `json:"status"`
}

type WSMessage struct {
	Type      string `json:"type"`
	PromptID  string `json:"prompt_id,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
	Response  string `json:"response,omitempty"`
	TokensIn  int    `json:"tokens_in,omitempty"`
	TokensOut int    `json:"tokens_out,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Hub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]chan []byte
}

func newHub() *Hub {
	return &Hub{
		clients: make(map[*websocket.Conn]chan []byte),
	}
}

func (h *Hub) register(conn *websocket.Conn) chan []byte {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.clients[conn] = ch
	h.mu.Unlock()
	return ch
}

func (h *Hub) unregister(conn *websocket.Conn) {
	h.mu.Lock()
	if ch, ok := h.clients[conn]; ok {
		delete(h.clients, conn)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *Hub) broadcast(msg WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.clients {
		select {
		case ch <- data:
		default:
		}
	}
}

type App struct {
	cfg     Config
	db      *sql.DB
	hub     *Hub
	notifyQ chan struct{}
}

func initDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(initSQL); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if _, err := db.Exec(`UPDATE queue SET status = 'pending', updated_at = datetime('now') WHERE status = 'processing'`); err != nil {
		return nil, fmt.Errorf("reset stale jobs: %w", err)
	}
	return db, nil
}

func (a *App) enqueuePrompt(ctx context.Context, promptText string) (PromptRecord, error) {
	id := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return PromptRecord{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO prompts (id, created_at, prompt_text) VALUES (?, ?, ?)`,
		id, now, promptText,
	); err != nil {
		return PromptRecord{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO queue (prompt_id, status, updated_at) VALUES (?, 'pending', ?)`,
		id, now,
	); err != nil {
		return PromptRecord{}, err
	}

	if err := tx.Commit(); err != nil {
		return PromptRecord{}, err
	}

	rec := PromptRecord{
		ID:        id,
		Timestamp: now,
		Prompt:    promptText,
		Status:    "pending",
	}

	a.hub.broadcast(WSMessage{
		Type:      "queued",
		PromptID:  rec.ID,
		Prompt:    rec.Prompt,
		Timestamp: rec.Timestamp,
	})

	select {
	case a.notifyQ <- struct{}{}:
	default:
	}

	return rec, nil
}

func (a *App) runQueueWorker(ctx context.Context) {
	client := &http.Client{Timeout: 180 * time.Second}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		for a.processNextPrompt(ctx, client) {
		}

		select {
		case <-ctx.Done():
			return
		case <-a.notifyQ:
		case <-ticker.C:
		}
	}
}

func (a *App) processNextPrompt(ctx context.Context, client *http.Client) bool {
	var promptID, promptText string
	err := a.db.QueryRowContext(ctx, `
		SELECT p.id, p.prompt_text
		FROM queue q
		JOIN prompts p ON p.id = q.prompt_id
		WHERE q.status = 'pending'
		ORDER BY q.seq ASC
		LIMIT 1
	`).Scan(&promptID, &promptText)
	if err != nil {
		return false
	}

	_, _ = a.db.ExecContext(ctx, `UPDATE queue SET status = 'processing', updated_at = datetime('now') WHERE prompt_id = ?`, promptID)
	a.hub.broadcast(WSMessage{Type: "processing", PromptID: promptID})

	respText, tokIn, tokOut, err := a.callOllama(ctx, client, promptText)
	if err != nil {
		log.Printf("ollama error for %s: %v", promptID, err)
		_, _ = a.db.ExecContext(ctx, `UPDATE queue SET status = 'failed', last_error = ?, updated_at = datetime('now') WHERE prompt_id = ?`, err.Error(), promptID)
		a.hub.broadcast(WSMessage{Type: "error", PromptID: promptID, Error: err.Error()})
		return true
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return true
	}
	defer tx.Rollback()

	_, _ = tx.ExecContext(ctx, `
		UPDATE prompts
		SET response_text = ?, tokens_in = ?, tokens_out = ?, completed_at = datetime('now')
		WHERE id = ?
	`, respText, tokIn, tokOut, promptID)

	_, _ = tx.ExecContext(ctx, `
		UPDATE queue
		SET status = 'ollama_done', updated_at = datetime('now')
		WHERE prompt_id = ?
	`, promptID)

	if err := tx.Commit(); err != nil {
		log.Printf("commit error for %s: %v", promptID, err)
		return true
	}

	a.hub.broadcast(WSMessage{
		Type:      "completed",
		PromptID:  promptID,
		Response:  respText,
		TokensIn:  tokIn,
		TokensOut: tokOut,
	})

	return true
}

func (a *App) callOllama(ctx context.Context, client *http.Client, prompt string) (string, int, int, error) {
	body, _ := json.Marshal(map[string]any{
		"model":  a.cfg.OllamaModel,
		"prompt": prompt,
		"stream": false,
		"think":  false,
		"options": map[string]any{
			"num_predict": a.cfg.NumPredict,
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.OllamaURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	res, err := client.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer res.Body.Close()

	if res.StatusCode >= 300 {
		return "", 0, 0, fmt.Errorf("ollama returned status %d", res.StatusCode)
	}

	var out struct {
		Response        string `json:"response"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		EvalCount       int    `json:"eval_count"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", 0, 0, err
	}
	return out.Response, out.PromptEvalCount, out.EvalCount, nil
}

func (a *App) runOutboxRelay(ctx context.Context) {
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for a.deliverNextToTD(ctx, client) {
			}
		}
	}
}

func (a *App) deliverNextToTD(ctx context.Context, client *http.Client) bool {
	var promptID, timestamp, promptText string
	var tokIn, tokOut int

	err := a.db.QueryRowContext(ctx, `
		SELECT p.id, p.created_at, p.prompt_text, p.tokens_in, p.tokens_out
		FROM queue q
		JOIN prompts p ON p.id = q.prompt_id
		WHERE q.status = 'ollama_done'
		ORDER BY q.seq ASC
		LIMIT 1
	`).Scan(&promptID, &timestamp, &promptText, &tokIn, &tokOut)
	if err != nil {
		return false
	}

	payload, _ := json.Marshal(map[string]any{
		"prompt_id":  promptID,
		"timestamp":  timestamp,
		"prompt":     promptText,
		"tokens_in":  tokIn,
		"tokens_out": tokOut,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.TDURL, bytes.NewReader(payload))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := client.Do(req)
	if err != nil {
		_, _ = a.db.ExecContext(ctx, `UPDATE queue SET attempts = attempts + 1, last_error = ?, updated_at = datetime('now') WHERE prompt_id = ?`, err.Error(), promptID)
		return false
	}
	res.Body.Close()

	if res.StatusCode >= 300 {
		_, _ = a.db.ExecContext(ctx, `UPDATE queue SET attempts = attempts + 1, last_error = ?, updated_at = datetime('now') WHERE prompt_id = ?`, fmt.Sprintf("TD HTTP %d", res.StatusCode), promptID)
		return false
	}

	_, _ = a.db.ExecContext(ctx, `UPDATE queue SET status = 'delivered', updated_at = datetime('now') WHERE prompt_id = ?`, promptID)
	return true
}

func (a *App) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	conn.SetReadLimit(16384)
	sendCh := a.hub.register(conn)
	defer a.hub.unregister(conn)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Writer & heartbeat ping loop
	go func() {
		pingTicker := time.NewTicker(25 * time.Second)
		defer pingTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-sendCh:
				if !ok {
					return
				}
				writeCtx, writeCancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(writeCtx, websocket.MessageText, msg)
				writeCancel()
				if err != nil {
					cancel()
					return
				}
			case <-pingTicker.C:
				pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pingCtx)
				pingCancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Reader loop
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var incoming struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(raw, &incoming); err != nil || incoming.Prompt == "" {
			continue
		}
		if _, err := a.enqueuePrompt(ctx, incoming.Prompt); err != nil {
			log.Printf("enqueue error: %v", err)
		}
	}

	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT p.id, p.created_at, p.prompt_text, p.response_text, p.tokens_in, p.tokens_out, q.status
		FROM prompts p
		JOIN queue q ON q.prompt_id = p.id
		ORDER BY q.seq ASC
		LIMIT 200
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	list := make([]PromptRecord, 0)
	for rows.Next() {
		var rec PromptRecord
		if err := rows.Scan(&rec.ID, &rec.Timestamp, &rec.Prompt, &rec.Response, &rec.TokensIn, &rec.TokensOut, &rec.Status); err == nil {
			list = append(list, rec)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (a *App) handleTDRefresh(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT p.id, p.created_at, p.prompt_text, p.tokens_in, p.tokens_out
		FROM prompts p
		JOIN queue q ON q.prompt_id = p.id
		WHERE q.status IN ('ollama_done', 'delivered')
		ORDER BY q.seq ASC
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type tdEvent struct {
		PromptID  string `json:"prompt_id"`
		Timestamp string `json:"timestamp"`
		Prompt    string `json:"prompt"`
		TokensIn  int    `json:"tokens_in"`
		TokensOut int    `json:"tokens_out"`
	}

	events := make([]tdEvent, 0)
	var totalIn, totalOut int

	for rows.Next() {
		var ev tdEvent
		if err := rows.Scan(&ev.PromptID, &ev.Timestamp, &ev.Prompt, &ev.TokensIn, &ev.TokensOut); err == nil {
			totalIn += ev.TokensIn
			totalOut += ev.TokensOut
			events = append(events, ev)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"total_tokens_in":  totalIn,
		"total_tokens_out": totalOut,
		"events":           events,
	})
}

func main() {
	cfg := loadConfig()
	db, err := initDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("db init failed: %v", err)
	}
	defer db.Close()

	app := &App{
		cfg:     cfg,
		db:      db,
		hub:     newHub(),
		notifyQ: make(chan struct{}, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go app.runQueueWorker(ctx)
	go app.runOutboxRelay(ctx)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	r.Get("/api/history", app.handleHistory)
	r.Get("/api/td/refresh", app.handleTDRefresh)
	r.Get("/ws", app.handleWS)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	go func() {
		log.Printf("backend listening on :%s -> Ollama %s (%s), TD %s", cfg.Port, cfg.OllamaURL, cfg.OllamaModel, cfg.TDURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}
