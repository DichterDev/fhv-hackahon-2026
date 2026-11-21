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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

//go:embed init.sql
var initSQL string

const waterCostFactorML = 0.001

type Config struct {
	Port        string
	DBPath      string
	OllamaURL   string
	OllamaModel string
	NumPredict  int
	TDURL       string
}

func loadConfig() Config {
	llmHost := getEnv("HACK_LLM", "100.64.0.4")
	llmPort := getEnv("HACK_LLM_PORT", "11434")
	tdHost := getEnv("HACK_TD", "100.64.0.5")
	tdPort := getEnv("HACK_TD_PORT", "8000")

	return Config{
		Port:        getEnv("PORT", "8080"),
		DBPath:      getEnv("DB_PATH", "/data/hackathon.db"),
		OllamaURL:   fmt.Sprintf("http://%s:%s/api/generate", llmHost, llmPort),
		OllamaModel: getEnv("OLLAMA_MODEL", "qwen3:8b"),
		NumPredict:  getEnvInt("OLLAMA_NUM_PREDICT", 150),
		TDURL:       fmt.Sprintf("http://%s:%s/api/visualization/events", tdHost, tdPort),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if val := os.Getenv(key); val != "" {
		if n, err := strconv.Atoi(val); err == nil {
			return n
		}
	}
	return fallback
}

type WSClient struct {
	conn *websocket.Conn
}

type Hub struct {
	mu      sync.RWMutex
	clients map[*WSClient]struct{}
}

func newHub() *Hub {
	return &Hub{clients: make(map[*WSClient]struct{})}
}

func (h *Hub) add(c *WSClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *WSClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *Hub) broadcast(msg any) {
	h.mu.RLock()
	clients := make([]*WSClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = wsjson.Write(ctx, c.conn, msg)
		cancel()
	}
}

type App struct {
	cfg     Config
	db      *sql.DB
	hub     *Hub
	notifyQ chan struct{}
}

type WSOutgoing struct {
	Type        string  `json:"type"` // "queued" | "processing" | "completed" | "error"
	PromptID    string  `json:"prompt_id,omitempty"`
	Prompt      string  `json:"prompt,omitempty"`
	Response    string  `json:"response,omitempty"`
	TokensIn    int     `json:"tokens_in,omitempty"`
	TokensOut   int     `json:"tokens_out,omitempty"`
	WaterCostML float64 `json:"water_cost_ml,omitempty"`
	Timestamp   string  `json:"timestamp,omitempty"`
	Error       string  `json:"error,omitempty"`
}

type TDVisualizationEvent struct {
	EventID      string  `json:"event_id"`
	PromptID     string  `json:"prompt_id"`
	Type         string  `json:"type"`
	Prompt       string  `json:"prompt"`
	Answer       string  `json:"answer"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	WaterML      float64 `json:"water_ml"`
}

func main() {
	cfg := loadConfig()

	db, err := sql.Open("sqlite", cfg.DBPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		log.Fatalf("failed to open sqlite: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(initSQL); err != nil {
		log.Fatalf("failed to initialize schema: %v", err)
	}

	_, _ = db.Exec(`UPDATE queue SET status = 'pending', updated_at = ? WHERE status = 'processing'`, time.Now().UTC().Format(time.RFC3339))

	app := &App{
		cfg:     cfg,
		db:      db,
		hub:     newHub(),
		notifyQ: make(chan struct{}, 1),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go app.runQueueWorker(ctx)
	go app.runOutboxRelay(ctx)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	r.Post("/api/prompts", app.handlePostPrompt)
	r.Get("/api/history", app.handleGetHistory)
	r.Get("/api/td/refresh", app.handleTDRefresh)
	r.Get("/ws", app.handleWS)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	go func() {
		log.Printf("backend listening on :%s (LLM=%s, model=%s, num_predict=%d, TD=%s)", cfg.Port, cfg.OllamaURL, cfg.OllamaModel, cfg.NumPredict, cfg.TDURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server error: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// --- HTTP Prompt Submission ---

func (a *App) handlePostPrompt(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"error":"invalid JSON payload"}`, http.StatusBadRequest)
		return
	}

	promptText := strings.TrimSpace(in.Prompt)
	if promptText == "" {
		http.Error(w, `{"error":"prompt must not be empty"}`, http.StatusBadRequest)
		return
	}

	id := "prompt-" + uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)

	if err := a.enqueuePrompt(id, now, promptText); err != nil {
		log.Printf("enqueue failed: %v", err)
		http.Error(w, `{"error":"failed to queue prompt"}`, http.StatusInternalServerError)
		return
	}

	a.hub.broadcast(WSOutgoing{
		Type:      "queued",
		PromptID:  id,
		Prompt:    promptText,
		Timestamp: now,
	})

	select {
	case a.notifyQ <- struct{}{}:
	default:
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":        id,
		"timestamp": now,
		"prompt":    promptText,
		"status":    "pending",
	})
}

func (a *App) enqueuePrompt(id, ts, prompt string) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT INTO prompts (id, timestamp, prompt) VALUES (?, ?, ?)`, id, ts, prompt); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO queue (prompt_id, status, updated_at) VALUES (?, 'pending', ?)`, id, ts); err != nil {
		return err
	}
	return tx.Commit()
}

// --- Read-Only Upstream WebSocket ---

func (a *App) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("ws accept error: %v", err)
		return
	}
	defer conn.CloseNow()

	client := &WSClient{conn: conn}
	a.hub.add(client)
	defer a.hub.remove(client)

	pingCtx, cancelPing := context.WithCancel(context.Background())
	defer cancelPing()
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(pingCtx, 5*time.Second)
				err := conn.Ping(ctx)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}()

	// Drain control frames until client disconnects
	for {
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
	}
}

// --- History & TouchDesigner Refresh Endpoints ---

func (a *App) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`
		SELECT p.id, p.timestamp, p.prompt, COALESCE(p.response, ''), p.tokens_in, p.tokens_out, p.water_cost_ml, q.status
		FROM prompts p
		JOIN queue q ON p.id = q.prompt_id
		ORDER BY q.seq ASC
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type item struct {
		ID          string  `json:"id"`
		Timestamp   string  `json:"timestamp"`
		Prompt      string  `json:"prompt"`
		Response    string  `json:"response"`
		TokensIn    int     `json:"tokens_in"`
		TokensOut   int     `json:"tokens_out"`
		WaterCostML float64 `json:"water_cost_ml"`
		Status      string  `json:"status"`
	}
	var list []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ID, &it.Timestamp, &it.Prompt, &it.Response, &it.TokensIn, &it.TokensOut, &it.WaterCostML, &it.Status); err == nil {
			list = append(list, it)
		}
	}
	if list == nil {
		list = []item{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (a *App) handleTDRefresh(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`
		SELECT q.seq, p.id, p.prompt, COALESCE(p.response, ''), p.tokens_in, p.tokens_out, p.water_cost_ml
		FROM prompts p
		JOIN queue q ON p.id = q.prompt_id
		WHERE q.status IN ('ollama_done', 'delivered')
		ORDER BY q.seq ASC
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var events []TDVisualizationEvent
	totalIn, totalOut := 0, 0
	totalWaterML := 0.0
	for rows.Next() {
		var seq int64
		var ev TDVisualizationEvent
		if err := rows.Scan(&seq, &ev.PromptID, &ev.Prompt, &ev.Answer, &ev.InputTokens, &ev.OutputTokens, &ev.WaterML); err == nil {
			ev.EventID = fmt.Sprintf("evt-%d", seq)
			ev.Type = "generation_completed"
			ev.TotalTokens = ev.InputTokens + ev.OutputTokens
			events = append(events, ev)

			totalIn += ev.InputTokens
			totalOut += ev.OutputTokens
			totalWaterML += ev.WaterML
		}
	}
	if events == nil {
		events = []TDVisualizationEvent{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"total_tokens_in":     totalIn,
		"total_tokens_out":    totalOut,
		"total_tokens":        totalIn + totalOut,
		"total_water_cost_ml": totalWaterML,
		"count":               len(events),
		"events":              events,
	})
}

// --- Stage 1 Worker: Sequential Ollama Inference ---

func (a *App) runQueueWorker(ctx context.Context) {
	httpClient := &http.Client{Timeout: 180 * time.Second}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		a.processNextPending(ctx, httpClient)
		select {
		case <-ctx.Done():
			return
		case <-a.notifyQ:
		case <-ticker.C:
		}
	}
}

func (a *App) processNextPending(ctx context.Context, client *http.Client) {
	for {
		var promptID, promptText, ts string
		err := a.db.QueryRow(`
			SELECT p.id, p.prompt, p.timestamp
			FROM queue q
			JOIN prompts p ON p.id = q.prompt_id
			WHERE q.status = 'pending'
			ORDER BY q.seq ASC
			LIMIT 1
		`).Scan(&promptID, &promptText, &ts)
		if err == sql.ErrNoRows {
			return
		}
		if err != nil {
			log.Printf("queue query error: %v", err)
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		_, _ = a.db.Exec(`UPDATE queue SET status = 'processing', updated_at = ? WHERE prompt_id = ?`, now, promptID)
		a.hub.broadcast(WSOutgoing{Type: "processing", PromptID: promptID})

		respText, tokIn, tokOut, err := a.callOllama(ctx, client, promptText)
		now = time.Now().UTC().Format(time.RFC3339)
		if err != nil {
			log.Printf("ollama error for %s: %v", promptID, err)
			_, _ = a.db.Exec(`UPDATE queue SET status = 'failed', last_error = ?, updated_at = ? WHERE prompt_id = ?`, err.Error(), now, promptID)
			a.hub.broadcast(WSOutgoing{Type: "error", PromptID: promptID, Error: err.Error()})
			continue
		}

		waterCostML := float64(tokIn+tokOut) * waterCostFactorML

		tx, err := a.db.Begin()
		if err == nil {
			_, _ = tx.Exec(`UPDATE prompts SET response = ?, tokens_in = ?, tokens_out = ?, water_cost_ml = ? WHERE id = ?`, respText, tokIn, tokOut, waterCostML, promptID)
			_, _ = tx.Exec(`UPDATE queue SET status = 'ollama_done', has_token_in = 1, has_token_out = 1, updated_at = ? WHERE prompt_id = ?`, now, promptID)
			_ = tx.Commit()
		}

		a.hub.broadcast(WSOutgoing{
			Type:        "completed",
			PromptID:    promptID,
			Prompt:      promptText,
			Response:    respText,
			TokensIn:    tokIn,
			TokensOut:   tokOut,
			WaterCostML: waterCostML,
			Timestamp:   ts,
		})
	}
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

// --- Stage 2 Worker: Sequential Outbox Relay to TouchDesigner ---

func (a *App) runOutboxRelay(ctx context.Context) {
	httpClient := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.flushOutboxToTD(ctx, httpClient)
		}
	}
}

func (a *App) flushOutboxToTD(ctx context.Context, client *http.Client) {
	rows, err := a.db.Query(`
		SELECT q.seq, p.id, p.prompt, COALESCE(p.response, ''), p.tokens_in, p.tokens_out, p.water_cost_ml
		FROM queue q
		JOIN prompts p ON p.id = q.prompt_id
		WHERE q.status = 'ollama_done'
		ORDER BY q.seq ASC
		LIMIT 10
	`)
	if err != nil {
		return
	}
	var batch []TDVisualizationEvent
	for rows.Next() {
		var seq int64
		var ev TDVisualizationEvent
		if err := rows.Scan(&seq, &ev.PromptID, &ev.Prompt, &ev.Answer, &ev.InputTokens, &ev.OutputTokens, &ev.WaterML); err == nil {
			ev.EventID = fmt.Sprintf("evt-%d", seq)
			ev.Type = "generation_completed"
			ev.TotalTokens = ev.InputTokens + ev.OutputTokens
			batch = append(batch, ev)
		}
	}
	rows.Close()

	for _, ev := range batch {
		payload, _ := json.Marshal(ev)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.TDURL, bytes.NewReader(payload))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		res, err := client.Do(req)
		now := time.Now().UTC().Format(time.RFC3339)
		if err != nil || res.StatusCode >= 300 {
			errMsg := "http error"
			if err != nil {
				errMsg = err.Error()
			} else {
				res.Body.Close()
				errMsg = fmt.Sprintf("status %d", res.StatusCode)
			}
			_, _ = a.db.Exec(`UPDATE queue SET attempts = attempts + 1, last_error = ?, updated_at = ? WHERE prompt_id = ?`, errMsg, now, ev.PromptID)
			return
		}
		res.Body.Close()

		_, _ = a.db.Exec(`UPDATE queue SET status = 'delivered', updated_at = ? WHERE prompt_id = ?`, now, ev.PromptID)
	}
}
