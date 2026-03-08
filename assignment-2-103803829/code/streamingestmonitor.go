package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type monitorConfig struct {
	listenAddr          string
	managerAlertURL     string
	minThroughputRPS    float64
	maxAvgBatchIngestMS float64
	alertCooldown       time.Duration
}

type workerPerformanceReport struct {
	TenantID                string  `json:"tenant_id"`
	WorkerID                string  `json:"worker_id"`
	KafkaTopic              string  `json:"kafka_topic"`
	ReportedAt              string  `json:"reported_at"`
	WindowSeconds           float64 `json:"window_seconds"`
	RecordsInWindow         int     `json:"records_in_window"`
	BatchesInWindow         int     `json:"batches_in_window"`
	AvgBatchIngestMS        float64 `json:"avg_batch_ingest_ms"`
	ThroughputRecordsPerSec float64 `json:"throughput_records_per_sec"`
	TotalInserted           int     `json:"total_inserted"`
	TotalConsumed           int     `json:"total_consumed"`
}

type alertThresholds struct {
	MinThroughputRPS    float64 `json:"min_throughput_rps"`
	MaxAvgBatchIngestMS float64 `json:"max_avg_batch_ingest_ms"`
}

type monitorAlert struct {
	TenantID    string                  `json:"tenant_id"`
	WorkerID    string                  `json:"worker_id"`
	TriggeredAt string                  `json:"triggered_at"`
	Severity    string                  `json:"severity"`
	Reasons     []string                `json:"reasons"`
	Thresholds  alertThresholds         `json:"thresholds"`
	Report      workerPerformanceReport `json:"report"`
}

type monitorState struct {
	mu                sync.Mutex
	lastAlertByTenant map[string]time.Time
}

func main() {
	cfg := loadMonitorConfig()
	state := &monitorState{lastAlertByTenant: make(map[string]time.Time)}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/reports", func(w http.ResponseWriter, r *http.Request) {
		handleReport(w, r, cfg, state)
	})

	log.Printf(
		"mysimbdp-streamingestmonitor listening on %s (manager=%s min_rps=%.2f max_batch_ms=%.2f cooldown=%s)",
		cfg.listenAddr,
		cfg.managerAlertURL,
		cfg.minThroughputRPS,
		cfg.maxAvgBatchIngestMS,
		cfg.alertCooldown,
	)

	if err := http.ListenAndServe(cfg.listenAddr, mux); err != nil {
		log.Fatalf("streamingestmonitor failed: %v", err)
	}
}

func loadMonitorConfig() monitorConfig {
	listenAddr := getEnvOrDefault("MONITOR_LISTEN_ADDR", ":8081")
	managerAlertURL := getEnvOrDefault("MANAGER_ALERT_URL", "http://streamingestmanager:8082/alerts")
	minThroughput := parseFloatEnv("MONITOR_MIN_THROUGHPUT_RPS", 300.0)
	maxAvgBatchMS := parseFloatEnv("MONITOR_MAX_AVG_BATCH_INGEST_MS", 250.0)
	cooldownSeconds := parseIntEnv("MONITOR_ALERT_COOLDOWN_SECONDS", 30)

	if cooldownSeconds < 0 {
		cooldownSeconds = 0
	}

	return monitorConfig{
		listenAddr:          listenAddr,
		managerAlertURL:     managerAlertURL,
		minThroughputRPS:    minThroughput,
		maxAvgBatchIngestMS: maxAvgBatchMS,
		alertCooldown:       time.Duration(cooldownSeconds) * time.Second,
	}
}

func handleReport(w http.ResponseWriter, r *http.Request, cfg monitorConfig, state *monitorState) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	defer r.Body.Close()

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	var report workerPerformanceReport
	if err := decoder.Decode(&report); err != nil {
		http.Error(w, fmt.Sprintf("invalid report payload: %v", err), http.StatusBadRequest)
		return
	}

	report.TenantID = strings.TrimSpace(report.TenantID)
	report.WorkerID = strings.TrimSpace(report.WorkerID)

	if report.TenantID == "" {
		http.Error(w, "tenant_id is required", http.StatusBadRequest)
		return
	}
	if report.WorkerID == "" {
		report.WorkerID = "unknown-worker"
	}

	log.Printf(
		"report received: tenant=%s worker=%s throughput=%.2f rps avg_batch_ms=%.2f window=%.1fs records=%d",
		report.TenantID,
		report.WorkerID,
		report.ThroughputRecordsPerSec,
		report.AvgBatchIngestMS,
		report.WindowSeconds,
		report.RecordsInWindow,
	)

	reasons := evaluateThresholds(report, cfg)
	alertForwarded := false
	cooldownActive := false

	if len(reasons) > 0 {
		if isAlertInCooldown(state, report.TenantID, cfg.alertCooldown) {
			cooldownActive = true
			log.Printf("alert skipped due to cooldown: tenant=%s worker=%s", report.TenantID, report.WorkerID)
		} else {
			alert := monitorAlert{
				TenantID:    report.TenantID,
				WorkerID:    report.WorkerID,
				TriggeredAt: time.Now().UTC().Format(time.RFC3339),
				Severity:    "warning",
				Reasons:     reasons,
				Thresholds: alertThresholds{
					MinThroughputRPS:    cfg.minThroughputRPS,
					MaxAvgBatchIngestMS: cfg.maxAvgBatchIngestMS,
				},
				Report: report,
			}

			if err := forwardAlert(cfg.managerAlertURL, alert); err != nil {
				log.Printf("failed to forward alert tenant=%s worker=%s: %v", report.TenantID, report.WorkerID, err)
			} else {
				alertForwarded = true
				markAlertSent(state, report.TenantID)
				log.Printf("alert forwarded: tenant=%s worker=%s reasons=%s", report.TenantID, report.WorkerID, strings.Join(reasons, " | "))
			}
		}
	}

	response := map[string]any{
		"status":          "accepted",
		"under_threshold": len(reasons) > 0,
		"reasons":         reasons,
		"cooldown_active": cooldownActive,
		"alert_forwarded": alertForwarded,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(response)
}

func evaluateThresholds(report workerPerformanceReport, cfg monitorConfig) []string {
	reasons := make([]string, 0, 2)

	if cfg.minThroughputRPS > 0 && report.ThroughputRecordsPerSec < cfg.minThroughputRPS {
		reasons = append(
			reasons,
			fmt.Sprintf("throughput %.2f rps below minimum %.2f rps", report.ThroughputRecordsPerSec, cfg.minThroughputRPS),
		)
	}

	if cfg.maxAvgBatchIngestMS > 0 && report.AvgBatchIngestMS > cfg.maxAvgBatchIngestMS {
		reasons = append(
			reasons,
			fmt.Sprintf("avg batch ingest %.2f ms above maximum %.2f ms", report.AvgBatchIngestMS, cfg.maxAvgBatchIngestMS),
		)
	}

	return reasons
}

func forwardAlert(url string, alert monitorAlert) error {
	payload, err := json.Marshal(alert)
	if err != nil {
		return fmt.Errorf("failed to marshal alert: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to build alert request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call manager alert endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("manager alert endpoint returned status %d", resp.StatusCode)
	}

	return nil
}

func isAlertInCooldown(state *monitorState, tenantID string, cooldown time.Duration) bool {
	if cooldown <= 0 {
		return false
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	lastAlertAt, ok := state.lastAlertByTenant[tenantID]
	if !ok {
		return false
	}

	return time.Since(lastAlertAt) < cooldown
}

func markAlertSent(state *monitorState, tenantID string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastAlertByTenant[tenantID] = time.Now()
}

func getEnvOrDefault(key string, defaultValue string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	return value
}

func parseFloatEnv(key string, defaultValue float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func parseIntEnv(key string, defaultValue int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}
