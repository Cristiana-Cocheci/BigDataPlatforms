package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type TenantSpec struct {
	TenantID          string
	Zookeeper         string
	Kafka             string
	WorkerService     string
	SourceService     string
	KafkaTopic        string
	CassandraKeyspace string
	SchemaProfile     string
}

type ManagerTenantConfig struct {
	TenantID       string `json:"tenant_id"`
	SourceCSV      string `json:"source_csv"`
	SourceChunkDir string `json:"source_chunk_dir"`
}

type WorkerPerformanceReport struct {
	TenantID                string  `json:"tenant_id"`
	WorkerID                string  `json:"worker_id"`
	KafkaTopic              string  `json:"kafka_topic"`
	ReportedAt              string  `json:"reported_at"`
	WindowSeconds           float64 `json:"window_seconds"`
	RecordsInWindow         int     `json:"records_in_window"`
	BatchesInWindow         int     `json:"batches_in_window"`
	AvgBatchIngestMS        float64 `json:"avg_batch_ingest_ms"`
	ThroughputRecordsPerSec float64 `json:"throughput_records_per_sec"`
	IngestedBytesInWindow   int64   `json:"ingested_bytes_in_window"`
	IngestedMBInWindow      float64 `json:"ingested_mb_in_window"`
	IngestedMBPerSec        float64 `json:"ingested_mb_per_sec"`
	TotalIngestedBytes      int64   `json:"total_ingested_bytes"`
	TotalIngestedMB         float64 `json:"total_ingested_mb"`
	TotalInserted           int     `json:"total_inserted"`
	TotalConsumed           int     `json:"total_consumed"`
}

type AlertThresholds struct {
	MinThroughputRPS    float64 `json:"min_throughput_rps"`
	MaxAvgBatchIngestMS float64 `json:"max_avg_batch_ingest_ms"`
}

type MonitorAlert struct {
	TenantID    string                  `json:"tenant_id"`
	WorkerID    string                  `json:"worker_id"`
	TriggeredAt string                  `json:"triggered_at"`
	Severity    string                  `json:"severity"`
	Reasons     []string                `json:"reasons"`
	Thresholds  AlertThresholds         `json:"thresholds"`
	Report      WorkerPerformanceReport `json:"report"`
}

var tenantRegistry = map[string]TenantSpec{
	"tenant1": {
		TenantID:          "tenant1",
		Zookeeper:         "zookeeper-tenant1",
		Kafka:             "kafka-tenant1",
		WorkerService:     "tenant1-streamingestworker",
		SourceService:     "tenant1-source",
		KafkaTopic:        "bme280-measurements",
		CassandraKeyspace: "mysimbdp_tenant1",
		SchemaProfile:     "bme280",
	},
	"tenant2": {
		TenantID:          "tenant2",
		Zookeeper:         "zookeeper-tenant2",
		Kafka:             "kafka-tenant2",
		WorkerService:     "tenant2-streamingestworker",
		SourceService:     "tenant2-source",
		KafkaTopic:        "dht22-measurements",
		CassandraKeyspace: "mysimbdp_tenant2",
		SchemaProfile:     "dht22",
	},
}

func main() {
	command := flag.String("command", "status", "Command: start | stop | status")
	tenantID := flag.String("tenant", "", "Tenant ID (tenant1 or tenant2). Empty with status means all tenants.")
	workerReplicas := flag.Int("workers", 1, "Number of streamingestworker replicas to run when command=start")
	sourceReplicas := flag.Int("source-replicas", 1, "Number of source producer replicas to run when command=start and --with-source=true")
	startSource := flag.Bool("with-source", false, "When command=start, also start tenant source simulator")
	prepareChunks := flag.Bool("prepare-chunks", false, "When command=start, run auxx/chunk_csv.py with num_chunks=workers using tenant source_csv")
	pythonBin := flag.String("python-bin", "python3", "Python executable used for --prepare-chunks")
	chunkScript := flag.String("chunk-script", "auxx/chunk_csv.py", "Path to chunking script used by --prepare-chunks")
	stopSource := flag.Bool("stop-source", true, "When command=stop, also stop tenant source simulator")
	stopBroker := flag.Bool("stop-broker", false, "When command=stop, also stop tenant Kafka+ZooKeeper")
	topicPartitions := flag.Int("partitions", 5, "Kafka topic partition count when command=start")
	alertListenAddr := flag.String("alert-listen-addr", ":8082", "Listen address for command=listen-alerts")
	composeFilesRaw := flag.String("compose-files", "docker-compose.yml,docker-compose.multitenant-brokers.yml", "Comma-separated docker compose files")

	flag.Parse()

	composeFiles := parseList(*composeFilesRaw)
	if len(composeFiles) == 0 {
		fatalf("compose-files cannot be empty")
	}

	switch *command {
	case "start":
		spec, err := resolveTenant(*tenantID)
		if err != nil {
			fatalf(err.Error())
		}
		if *workerReplicas < 1 {
			fatalf("workers must be >= 1")
		}
		if *sourceReplicas < 1 {
			fatalf("source-replicas must be >= 1")
		}
		if *topicPartitions < 1 {
			fatalf("partitions must be >= 1")
		}
		if err := startTenant(composeFiles, spec, *workerReplicas, *sourceReplicas, *startSource, *topicPartitions, *prepareChunks, *pythonBin, *chunkScript); err != nil {
			fatalf("start failed: %v", err)
		}
		fmt.Printf("tenant=%s started: workers=%d source_replicas=%d with-source=%t prepare-chunks=%t schema=%s\n", spec.TenantID, *workerReplicas, *sourceReplicas, *startSource, *prepareChunks, spec.SchemaProfile)

	case "stop":
		spec, err := resolveTenant(*tenantID)
		if err != nil {
			fatalf(err.Error())
		}
		if err := stopTenant(composeFiles, spec, *stopSource, *stopBroker); err != nil {
			fatalf("stop failed: %v", err)
		}
		fmt.Printf("tenant=%s stopped: stop-source=%t stop-broker=%t\n", spec.TenantID, *stopSource, *stopBroker)

	case "status":
		if strings.TrimSpace(*tenantID) == "" {
			if err := showAllStatus(composeFiles); err != nil {
				fatalf("status failed: %v", err)
			}
			return
		}

		spec, err := resolveTenant(*tenantID)
		if err != nil {
			fatalf(err.Error())
		}
		if err := showTenantStatus(composeFiles, spec); err != nil {
			fatalf("status failed: %v", err)
		}

	case "listen-alerts":
		if err := listenForMonitorAlerts(*alertListenAddr); err != nil {
			fatalf("listen-alerts failed: %v", err)
		}

	default:
		fatalf("invalid command: %s (expected: start | stop | status | listen-alerts)", *command)
	}
}

func startTenant(composeFiles []string, spec TenantSpec, workers int, sourceReplicas int, withSource bool, partitions int, prepareChunks bool, pythonBin string, chunkScript string) error {
	if sourceReplicas < 1 {
		sourceReplicas = 1
	}

	chunkCount := workers
	if withSource {
		chunkCount = sourceReplicas
	}

	if prepareChunks {
		if err := prepareTenantChunks(spec, chunkCount, pythonBin, chunkScript); err != nil {
			return err
		}
	}

	if withSource {
		if err := resetSourceChunkClaims(spec); err != nil {
			return err
		}
	}

	if err := composeUp(composeFiles,
		"cassandra1", "cassandra2", "cassandra3",
		spec.Zookeeper, spec.Kafka,
		"streamingestmanager", "streamingestmonitor",
	); err != nil {
		return err
	}

	if err := ensureTenantKeyspace(composeFiles, spec); err != nil {
		return err
	}

	if err := ensureTenantSchema(composeFiles, spec); err != nil {
		return err
	}

	if err := ensureTenantTopic(composeFiles, spec, partitions); err != nil {
		return err
	}

	if err := composeUpScale(composeFiles, spec.WorkerService, workers); err != nil {
		return err
	}

	if withSource {
		if err := composeUpScale(composeFiles, spec.SourceService, sourceReplicas); err != nil {
			return err
		}
	}

	return nil
}

func stopTenant(composeFiles []string, spec TenantSpec, stopSource bool, stopBroker bool) error {
	if err := composeStopRm(composeFiles, spec.WorkerService); err != nil {
		return err
	}

	if stopSource {
		if err := composeStopRm(composeFiles, spec.SourceService); err != nil {
			return err
		}
	}

	if stopBroker {
		if err := composeStopRm(composeFiles, spec.Kafka, spec.Zookeeper); err != nil {
			return err
		}
	}

	return nil
}

func showAllStatus(composeFiles []string) error {
	services := []string{}
	for _, spec := range tenantRegistry {
		services = append(services, spec.Zookeeper, spec.Kafka, spec.SourceService, spec.WorkerService)
	}
	services = append(services, "streamingestmanager", "streamingestmonitor")
	return composePs(composeFiles, services...)
}

func showTenantStatus(composeFiles []string, spec TenantSpec) error {
	return composePs(composeFiles, spec.Zookeeper, spec.Kafka, spec.SourceService, spec.WorkerService, "streamingestmanager", "streamingestmonitor")
}

func listenForMonitorAlerts(listenAddr string) error {
	trimmedAddr := strings.TrimSpace(listenAddr)
	if trimmedAddr == "" {
		return errors.New("alert-listen-addr cannot be empty")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		defer r.Body.Close()

		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		var alert MonitorAlert
		if err := decoder.Decode(&alert); err != nil {
			http.Error(w, fmt.Sprintf("invalid alert payload: %v", err), http.StatusBadRequest)
			return
		}

		tenantID := strings.TrimSpace(alert.TenantID)
		if tenantID == "" {
			http.Error(w, "tenant_id is required", http.StatusBadRequest)
			return
		}

		workerID := strings.TrimSpace(alert.WorkerID)
		if workerID == "" {
			workerID = "unknown"
		}

		severity := strings.TrimSpace(alert.Severity)
		if severity == "" {
			severity = "warning"
		}

		reasons := strings.TrimSpace(strings.Join(alert.Reasons, " | "))
		if reasons == "" {
			reasons = "unspecified"
		}

		fmt.Printf(
			"monitor alert received: tenant=%s worker=%s severity=%s reasons=%s throughput=%.2f avg_batch_ms=%.2f window_mb=%.4f total_mb=%.4f\n",
			tenantID,
			workerID,
			severity,
			reasons,
			alert.Report.ThroughputRecordsPerSec,
			alert.Report.AvgBatchIngestMS,
			alert.Report.IngestedMBInWindow,
			alert.Report.TotalIngestedMB,
		)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"acknowledged"}`))
	})

	fmt.Printf("mysimbdp-streamingestmanager alert receiver listening on %s\n", trimmedAddr)
	return http.ListenAndServe(trimmedAddr, mux)
}

func ensureTenantKeyspace(composeFiles []string, spec TenantSpec) error {
	query := fmt.Sprintf(
		"CREATE KEYSPACE IF NOT EXISTS %s WITH replication = {'class': 'NetworkTopologyStrategy', 'DC1': 2, 'DC2': 1};",
		spec.CassandraKeyspace,
	)

	args := composeBaseArgs(composeFiles)
	args = append(args, "exec", "-T", "cassandra1", "cqlsh", "-e", query)
	return run("docker", args...)
}

func ensureTenantTopic(composeFiles []string, spec TenantSpec, partitions int) error {
	args := composeBaseArgs(composeFiles)
	args = append(args,
		"exec", "-T", spec.Kafka,
		"kafka-topics",
		"--bootstrap-server", "localhost:29092",
		"--create",
		"--if-not-exists",
		"--topic", spec.KafkaTopic,
		"--partitions", strconv.Itoa(partitions),
		"--replication-factor", "1",
	)
	return run("docker", args...)
}

func ensureTenantSchema(composeFiles []string, spec TenantSpec) error {
	queries := []string{
		fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s.tenant_schema_registry (tenant_id text PRIMARY KEY, schema_profile text, updated_at timestamp);",
			spec.CassandraKeyspace,
		),
		fmt.Sprintf(
			"INSERT INTO %s.tenant_schema_registry (tenant_id, schema_profile, updated_at) VALUES ('%s', '%s', toTimestamp(now()));",
			spec.CassandraKeyspace,
			spec.TenantID,
			spec.SchemaProfile,
		),
	}

	for _, query := range queries {
		args := composeBaseArgs(composeFiles)
		args = append(args, "exec", "-T", "cassandra1", "cqlsh", "-e", query)
		if err := run("docker", args...); err != nil {
			return err
		}
	}

	return nil
}

func prepareTenantChunks(spec TenantSpec, workers int, pythonBin string, chunkScript string) error {
	if workers < 1 {
		return errors.New("workers must be >= 1 for chunk preparation")
	}

	if strings.TrimSpace(pythonBin) == "" {
		return errors.New("python-bin cannot be empty")
	}

	scriptPath := strings.TrimSpace(chunkScript)
	if scriptPath == "" {
		return errors.New("chunk-script cannot be empty")
	}

	if _, err := os.Stat(scriptPath); err != nil {
		return fmt.Errorf("chunk preparation failed: script not found at %s: %w", scriptPath, err)
	}

	tenantConfig, err := loadManagerTenantConfig(spec.TenantID)
	if err != nil {
		return err
	}

	hostCSVPath := mapContainerDataPathToHost(tenantConfig.SourceCSV)
	if _, err := os.Stat(hostCSVPath); err != nil {
		return fmt.Errorf("chunk preparation failed: source csv not found at %s: %w", hostCSVPath, err)
	}

	fmt.Printf("Preparing chunks: tenant=%s source=%s chunks=%d\n", spec.TenantID, hostCSVPath, workers)
	if err := run(strings.TrimSpace(pythonBin), scriptPath, hostCSVPath, strconv.Itoa(workers)); err != nil {
		return fmt.Errorf("chunk preparation failed: %w", err)
	}

	if err := resetSourceChunkClaims(spec); err != nil {
		return err
	}

	return nil
}

func resetSourceChunkClaims(spec TenantSpec) error {
	tenantConfig, err := loadManagerTenantConfig(spec.TenantID)
	if err != nil {
		return err
	}

	hostChunkDir := mapContainerDataPathToHost(tenantConfig.SourceChunkDir)
	hostChunkDir = strings.TrimSpace(hostChunkDir)
	if hostChunkDir == "" {
		hostCSVPath := mapContainerDataPathToHost(tenantConfig.SourceCSV)
		hostChunkDir = filepath.Join(filepath.Dir(hostCSVPath), "chunks")
	}

	claimsDir := filepath.Join(hostChunkDir, ".source_claims")
	if err := os.RemoveAll(claimsDir); err != nil {
		return fmt.Errorf("failed to reset source chunk claims at %s: %w", claimsDir, err)
	}

	return nil
}

func loadManagerTenantConfig(tenantID string) (ManagerTenantConfig, error) {
	normalizedTenantID := strings.ToLower(strings.TrimSpace(tenantID))
	if normalizedTenantID == "" {
		return ManagerTenantConfig{}, errors.New("tenant id is required for tenant config loading")
	}

	configDir := envOrDefault("TENANT_CONFIG_DIR", "./tenant_configs")
	configPath := filepath.Join(configDir, normalizedTenantID+".json")

	content, err := os.ReadFile(configPath)
	if err != nil {
		return ManagerTenantConfig{}, fmt.Errorf("failed to read tenant config %s: %w", configPath, err)
	}

	var config ManagerTenantConfig
	if err := json.Unmarshal(content, &config); err != nil {
		return ManagerTenantConfig{}, fmt.Errorf("failed to parse tenant config %s: %w", configPath, err)
	}

	if strings.TrimSpace(config.SourceCSV) == "" {
		return ManagerTenantConfig{}, fmt.Errorf("tenant config %s is missing source_csv", configPath)
	}

	if strings.TrimSpace(config.TenantID) == "" {
		config.TenantID = normalizedTenantID
	}

	return config, nil
}

func mapContainerDataPathToHost(path string) string {
	trimmedPath := strings.TrimSpace(path)
	if !strings.HasPrefix(trimmedPath, "/data/") {
		return trimmedPath
	}
	return filepath.Join("..", "data", strings.TrimPrefix(trimmedPath, "/data/"))
}

func envOrDefault(key string, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}

func composeUp(composeFiles []string, services ...string) error {
	args := composeBaseArgs(composeFiles)
	args = append(args, "up", "-d")
	args = append(args, services...)
	return run("docker", args...)
}

func composeUpScale(composeFiles []string, service string, replicas int) error {
	args := composeBaseArgs(composeFiles)
	args = append(args, "up", "-d", "--scale", fmt.Sprintf("%s=%d", service, replicas), service)
	return run("docker", args...)
}

func composeStopRm(composeFiles []string, services ...string) error {
	stopArgs := composeBaseArgs(composeFiles)
	stopArgs = append(stopArgs, "stop")
	stopArgs = append(stopArgs, services...)
	if err := run("docker", stopArgs...); err != nil {
		return err
	}

	rmArgs := composeBaseArgs(composeFiles)
	rmArgs = append(rmArgs, "rm", "-f")
	rmArgs = append(rmArgs, services...)
	return run("docker", rmArgs...)
}

func composePs(composeFiles []string, services ...string) error {
	args := composeBaseArgs(composeFiles)
	args = append(args, "ps")
	args = append(args, services...)
	return run("docker", args...)
}

func composeBaseArgs(composeFiles []string) []string {
	args := []string{"compose"}
	for _, file := range composeFiles {
		args = append(args, "-f", file)
	}
	return args
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func resolveTenant(tenantID string) (TenantSpec, error) {
	t := strings.TrimSpace(tenantID)
	if t == "" {
		return TenantSpec{}, errors.New("tenant is required for start/stop")
	}
	spec, ok := tenantRegistry[t]
	if !ok {
		return TenantSpec{}, fmt.Errorf("unknown tenant: %s", t)
	}
	return spec, nil
}

func parseList(input string) []string {
	parts := strings.Split(input, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
