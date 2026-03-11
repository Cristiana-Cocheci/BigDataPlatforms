package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gocql/gocql"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"gopkg.in/yaml.v3"
)

const (
	defaultCassandraKeyspace          = "mysimbdp_tenant2"
	defaultCassandraHosts             = "cassandra1,cassandra2,cassandra3"
	defaultTenantConfigDir            = "./tenant_configs"
	defaultTenantTier                 = "gold"
	tenantTierGold                    = "gold"
	tenantTierSilver                  = "silver"
	supportedSilverPipelineTenant     = "tenant2"
	defaultSilverPipelineTenant       = "tenant2"
	defaultSilverPipelinePageSize     = 1000
	defaultSilverPipelineRuntime      = 30 * time.Minute
	defaultSilverPipelineMaxRetries   = 3
	defaultSilverPipelineRetryBackoff = 500 * time.Millisecond
	maxSilverPipelineRetryBackoff     = 5 * time.Second
	defaultSilverPipelineNumConns     = 2
	defaultSilverPipelineCacheRoot    = "./tenant_caching_dir"
	defaultSilverPipelineStorage      = "local"
	defaultSilverPipelineLogRoot      = "./logs/silverpipeline"
	defaultSilverPipelineRunLogFile   = "run_status.jsonl"
	defaultSilverPipelineTaskLogFile  = "task_status.jsonl"
	defaultSilverSummarySuffix        = "silver_hourly"
	defaultBatchmanagerInputGlob      = "*_bronze_extract.csv"
	defaultBatchmanagerStateFile      = ".batchmanager_state.json"
	silverPipelineModeEnv             = "SILVER_PIPELINE_MODE"
	silverPipelineInputFilesEnv       = "SILVER_PIPELINE_INPUT_FILES"
	silverPipelineDayEnv              = "SILVER_PIPELINE_DAY"
	silverPipelineStorageBackendEnv   = "SILVER_PIPELINE_STORAGE_BACKEND"
	silverPipelineGCSBucketEnv        = "SILVER_PIPELINE_GCS_BUCKET"
	silverPipelineGCSPrefixEnv        = "SILVER_PIPELINE_GCS_PREFIX"
	silverPipelineGCSCredsFileEnv     = "SILVER_PIPELINE_GCS_CREDENTIALS_FILE"
	silverPipelineLogDirEnv           = "SILVER_PIPELINE_LOG_DIR"
	silverPipelineRunLogFileEnv       = "SILVER_PIPELINE_RUN_LOG_FILE"
	silverPipelineTaskLogFileEnv      = "SILVER_PIPELINE_TASK_LOG_FILE"
	silverPipelineModeFull            = "full"
	silverPipelineModeExtract         = "extract-cache"
	silverPipelineModeTransform       = "transform-cache"
	silverPipelineStorageLocal        = "local"
	silverPipelineStorageGCS          = "gcs"
	hardcodedTenant2ID                = "tenant2"
	hardcodedTenant2TablePrefix       = "sensor_observations"
	hardcodedTenant2TableSuffix       = "dht22"
	hardcodedTenant2BronzeTable       = "sensor_observations_dht22_bronze"
	hardcodedTenant2SilverTable       = "sensor_observations_dht22_silver"
	hardcodedTenant2TableSuffixField  = "sensor_type"
)

var (
	cassandraKeyspace       = defaultCassandraKeyspace
	cassandraHosts          = parseCSVList(getEnv("CASSANDRA_HOSTS", defaultCassandraHosts))
	hardcodedTenant2Columns = []TenantSchemaColumnConfig{
		{Name: "sensor_id", Type: "int", Field: "sensor_id"},
		{Name: "sensor_type", Type: "text", Field: "sensor_type"},
		{Name: "location", Type: "float", Field: "location"},
		{Name: "lat", Type: "float", Field: "lat"},
		{Name: "lon", Type: "float", Field: "lon"},
		{Name: "day", Type: "text", Field: "day"},
		{Name: "hour", Type: "int", Field: "hour"},
		{Name: "timestamp", Type: "text", Field: "timestamp"},
		{Name: "temperature", Type: "float", Field: "temperature"},
		{Name: "humidity", Type: "float", Field: "humidity"},
	}
	hardcodedTenant2MetricFields = []string{"temperature", "humidity"}
)

type TenantConfig struct {
	TenantID    string             `json:"tenant_id"`
	Tier        string             `json:"tier"`
	TablePrefix string             `json:"table_prefix"`
	Schema      TenantSchemaConfig `json:"schema"`
}

type TenantSchemaColumnConfig struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Field string `json:"field"`
}

type TenantPrimaryKeyConfig struct {
	Partition  []string `json:"partition"`
	Clustering []string `json:"clustering"`
}

type TenantSchemaConfig struct {
	Columns          []TenantSchemaColumnConfig `json:"columns"`
	PrimaryKey       TenantPrimaryKeyConfig     `json:"primary_key"`
	TableSuffixField string                     `json:"table_suffix_field"`
}

type silverPipelineConfig struct {
	TenantID            string                    `yaml:"tenant_id"`
	PipelineConstraints silverPipelineConstraints `yaml:"pipeline_constraints"`
	Pipeline            silverPipelineRuntime     `yaml:"pipeline"`
}

type silverPipelineConstraints struct {
	Compute struct {
		MaxCPUCores     int `yaml:"max_cpu_cores"`
		MaxMemoryGB     int `yaml:"max_memory_gb"`
		MaxParallelJobs int `yaml:"max_parallel_jobs"`
	} `yaml:"compute"`
	Throughput struct {
		MaxRecordsPerSecond int `yaml:"max_records_per_second"`
		MaxMBPerSecond      int `yaml:"max_mb_per_second"`
	} `yaml:"throughput"`
	Scheduling struct {
		PipelineType          string `yaml:"pipeline_type"`
		MinBatchIntervalSec   int    `yaml:"min_batch_interval_sec"`
		MaxPipelineRuntimeSec int    `yaml:"max_pipeline_runtime_sec"`
	} `yaml:"scheduling"`
	Storage struct {
		MaxSilverStorageGB  int `yaml:"max_silver_storage_gb"`
		SilverRetentionDays int `yaml:"silver_retention_days"`
	} `yaml:"storage"`
	Reliability struct {
		MaxRetries            int `yaml:"max_retries"`
		CheckpointIntervalSec int `yaml:"checkpoint_interval_sec"`
	} `yaml:"reliability"`
	Latency struct {
		MaxProcessingDelaySec int `yaml:"max_processing_delay_sec"`
	} `yaml:"latency"`
}

type silverPipelineRuntime struct {
	StorageBackend  string                       `yaml:"storage_backend"`
	CacheDir        string                       `yaml:"cache_dir"`
	Logging         silverPipelineLoggingRuntime `yaml:"logging"`
	ExtractPageSize int                          `yaml:"extract_page_size"`
	ExtractDay      string                       `yaml:"extract_day"`
	GCS             silverPipelineGCSRuntime     `yaml:"gcs"`
	Transformation  silverPipelineTransformation `yaml:"transformation"`
	BatchManager    silverPipelineBatchManager   `yaml:"batchmanager"`
}

type silverPipelineLoggingRuntime struct {
	Dir      string `yaml:"dir"`
	RunFile  string `yaml:"run_file"`
	TaskFile string `yaml:"task_file"`
}

type silverPipelineGCSRuntime struct {
	Bucket          string `yaml:"bucket"`
	Prefix          string `yaml:"prefix"`
	CredentialsFile string `yaml:"credentials_file"`
}

type silverPipelineTransformation struct {
	DropRowsWithMissingEntries *bool    `yaml:"drop_rows_with_missing_entries"`
	MetricFields               []string `yaml:"metric_fields"`
}

type silverPipelineBatchManager struct {
	InputGlob string `yaml:"input_glob"`
	StateFile string `yaml:"state_file"`
}

type hourlyGroupKey struct {
	Day  string
	Hour int
}

type metricAccumulator struct {
	Values []float64
	Sum    float64
	Min    float64
	Max    float64
}

type hourlyAccumulator struct {
	RecordCount int
	Metrics     map[string]*metricAccumulator
}

type metricSummary struct {
	Avg    float64
	Min    float64
	Max    float64
	Median float64
}

type silverAggregateRow struct {
	Day               string
	Hour              int
	RecordsAggregated int
	Metrics           map[string]metricSummary
}

type silverPipelineFileLogger struct {
	runID          string
	tenantID       string
	mode           string
	storageBackend string
	storageTarget  string
	runLogFile     *os.File
	taskLogFile    *os.File
}

var activeSilverPipelineLogger *silverPipelineFileLogger

func getEnv(key string, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}

func parseCSVList(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return []string{"cassandra1", "cassandra2", "cassandra3"}
	}
	return result
}

func loadTenantConfig(tenantID string) (TenantConfig, error) {
	normalizedTenantID := strings.ToLower(strings.TrimSpace(tenantID))
	if normalizedTenantID == "" {
		normalizedTenantID = hardcodedTenant2ID
	}
	if normalizedTenantID != hardcodedTenant2ID {
		return TenantConfig{}, fmt.Errorf("tenant2 pipeline only supports tenant=%s", hardcodedTenant2ID)
	}

	return TenantConfig{
		TenantID:    hardcodedTenant2ID,
		Tier:        tenantTierSilver,
		TablePrefix: hardcodedTenant2TablePrefix,
		Schema: TenantSchemaConfig{
			Columns:          hardcodedTenant2Columns,
			TableSuffixField: hardcodedTenant2TableSuffixField,
			PrimaryKey: TenantPrimaryKeyConfig{
				Partition:  []string{"day", "hour"},
				Clustering: []string{"sensor_id", "timestamp"},
			},
		},
	}, nil
}

func normalizeTenantTier(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case tenantTierGold:
		return tenantTierGold
	case tenantTierSilver:
		return tenantTierSilver
	default:
		return defaultTenantTier
	}
}

func activeSilverTenantID() string {
	tenantID := strings.ToLower(strings.TrimSpace(os.Getenv("TENANT_ID")))
	if tenantID == "" {
		return defaultSilverPipelineTenant
	}
	return tenantID
}

func ensureSupportedSilverTenant(tenantID string) error {
	if strings.ToLower(strings.TrimSpace(tenantID)) != supportedSilverPipelineTenant {
		return fmt.Errorf("silverpipeline currently supports only tenant=%s", supportedSilverPipelineTenant)
	}
	return nil
}

func silverPipelineMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(silverPipelineModeEnv))) {
	case "", silverPipelineModeFull:
		return silverPipelineModeFull
	case silverPipelineModeExtract:
		return silverPipelineModeExtract
	case silverPipelineModeTransform:
		return silverPipelineModeTransform
	default:
		return strings.ToLower(strings.TrimSpace(os.Getenv(silverPipelineModeEnv)))
	}
}

func normalizeStorageBackend(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", silverPipelineStorageLocal:
		return silverPipelineStorageLocal
	case silverPipelineStorageGCS:
		return silverPipelineStorageGCS
	default:
		return normalized
	}
}

func normalizeGCSPrefix(value string) string {
	return strings.Trim(strings.TrimSpace(value), "/")
}

func silverPipelineConfigPath(tenantID string) string {
	configDir := getEnv("TENANT_CONFIG_DIR", defaultTenantConfigDir)
	return filepath.Join(configDir, fmt.Sprintf("silverpipeline_%s.yaml", tenantID))
}

func defaultSilverPipelineLogDir(tenantID string) string {
	return filepath.Join(defaultSilverPipelineLogRoot, tenantID)
}

func loadSilverPipelineConfig(tenantID string) (silverPipelineConfig, error) {
	configPath := strings.TrimSpace(os.Getenv("SILVER_PIPELINE_CONFIG"))
	if configPath == "" {
		configPath = silverPipelineConfigPath(tenantID)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		return silverPipelineConfig{}, fmt.Errorf("failed to read silver pipeline config %s: %w", configPath, err)
	}

	var config silverPipelineConfig
	if err := yaml.Unmarshal(content, &config); err != nil {
		return silverPipelineConfig{}, fmt.Errorf("failed to parse silver pipeline config %s: %w", configPath, err)
	}

	config.applyDefaults(tenantID)

	if config.TenantID != tenantID {
		return silverPipelineConfig{}, fmt.Errorf("silver pipeline config tenant mismatch: expected=%s got=%s", tenantID, config.TenantID)
	}

	config.Pipeline.Transformation.MetricFields = append([]string(nil), hardcodedTenant2MetricFields...)

	return config, nil
}

func (config *silverPipelineConfig) applyDefaults(tenantID string) {
	if strings.TrimSpace(config.TenantID) == "" {
		config.TenantID = tenantID
	} else {
		config.TenantID = strings.ToLower(strings.TrimSpace(config.TenantID))
	}

	storageBackend := normalizeStorageBackend(config.Pipeline.StorageBackend)
	if envStorageBackend := strings.TrimSpace(os.Getenv(silverPipelineStorageBackendEnv)); envStorageBackend != "" {
		storageBackend = normalizeStorageBackend(envStorageBackend)
	}
	if storageBackend == "" {
		storageBackend = defaultSilverPipelineStorage
	}
	config.Pipeline.StorageBackend = storageBackend

	if strings.TrimSpace(config.Pipeline.CacheDir) == "" {
		config.Pipeline.CacheDir = filepath.Join(defaultSilverPipelineCacheRoot, tenantID)
	}

	if strings.TrimSpace(config.Pipeline.Logging.Dir) == "" {
		config.Pipeline.Logging.Dir = defaultSilverPipelineLogDir(tenantID)
	}
	if strings.TrimSpace(config.Pipeline.Logging.RunFile) == "" {
		config.Pipeline.Logging.RunFile = defaultSilverPipelineRunLogFile
	}
	if strings.TrimSpace(config.Pipeline.Logging.TaskFile) == "" {
		config.Pipeline.Logging.TaskFile = defaultSilverPipelineTaskLogFile
	}

	if envLogDir := strings.TrimSpace(os.Getenv(silverPipelineLogDirEnv)); envLogDir != "" {
		config.Pipeline.Logging.Dir = envLogDir
	}
	if envRunLogFile := strings.TrimSpace(os.Getenv(silverPipelineRunLogFileEnv)); envRunLogFile != "" {
		config.Pipeline.Logging.RunFile = envRunLogFile
	}
	if envTaskLogFile := strings.TrimSpace(os.Getenv(silverPipelineTaskLogFileEnv)); envTaskLogFile != "" {
		config.Pipeline.Logging.TaskFile = envTaskLogFile
	}

	if envBucket := strings.TrimSpace(os.Getenv(silverPipelineGCSBucketEnv)); envBucket != "" {
		config.Pipeline.GCS.Bucket = envBucket
	}
	if envPrefix := strings.TrimSpace(os.Getenv(silverPipelineGCSPrefixEnv)); envPrefix != "" {
		config.Pipeline.GCS.Prefix = envPrefix
	}
	if envCredsFile := strings.TrimSpace(os.Getenv(silverPipelineGCSCredsFileEnv)); envCredsFile != "" {
		config.Pipeline.GCS.CredentialsFile = envCredsFile
	}
	config.Pipeline.GCS.Prefix = normalizeGCSPrefix(config.Pipeline.GCS.Prefix)

	if config.Pipeline.ExtractPageSize < 1 {
		config.Pipeline.ExtractPageSize = defaultSilverPipelinePageSize
	}

	config.Pipeline.ExtractDay = strings.TrimSpace(config.Pipeline.ExtractDay)
	if envExtractDay := strings.TrimSpace(os.Getenv(silverPipelineDayEnv)); envExtractDay != "" {
		config.Pipeline.ExtractDay = envExtractDay
	}

	config.Pipeline.Transformation.MetricFields = append([]string(nil), hardcodedTenant2MetricFields...)

	if strings.TrimSpace(config.Pipeline.BatchManager.InputGlob) == "" {
		config.Pipeline.BatchManager.InputGlob = defaultBatchmanagerInputGlob
	}

	if strings.TrimSpace(config.Pipeline.BatchManager.StateFile) == "" {
		config.Pipeline.BatchManager.StateFile = defaultBatchmanagerStateFile
	}
}

func validateStorageBackendConfig(config silverPipelineConfig) error {
	switch config.Pipeline.StorageBackend {
	case silverPipelineStorageLocal:
		return nil
	case silverPipelineStorageGCS:
		if strings.TrimSpace(config.Pipeline.GCS.Bucket) == "" {
			return fmt.Errorf("missing GCS bucket: set pipeline.gcs.bucket or %s", silverPipelineGCSBucketEnv)
		}
		return nil
	default:
		return fmt.Errorf("unsupported storage backend %q (expected %q or %q)", config.Pipeline.StorageBackend, silverPipelineStorageLocal, silverPipelineStorageGCS)
	}
}

func storageTargetForLog(config silverPipelineConfig) string {
	if config.Pipeline.StorageBackend == silverPipelineStorageGCS {
		bucket := strings.TrimSpace(config.Pipeline.GCS.Bucket)
		prefix := normalizeGCSPrefix(config.Pipeline.GCS.Prefix)
		if prefix == "" {
			return fmt.Sprintf("gs://%s", bucket)
		}
		return fmt.Sprintf("gs://%s/%s", bucket, prefix)
	}
	return config.Pipeline.CacheDir
}

func isBareFileName(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	return filepath.Base(trimmed) == trimmed
}

func (config silverPipelineConfig) logOutputPaths() (string, string) {
	logDir := strings.TrimSpace(config.Pipeline.Logging.Dir)
	runFile := strings.TrimSpace(config.Pipeline.Logging.RunFile)
	taskFile := strings.TrimSpace(config.Pipeline.Logging.TaskFile)

	if runFile == "" {
		runFile = defaultSilverPipelineRunLogFile
	}
	if taskFile == "" {
		taskFile = defaultSilverPipelineTaskLogFile
	}
	if logDir == "" {
		logDir = defaultSilverPipelineLogDir(config.TenantID)
	}

	if isBareFileName(runFile) {
		runFile = filepath.Join(logDir, runFile)
	}
	if isBareFileName(taskFile) {
		taskFile = filepath.Join(logDir, taskFile)
	}

	return filepath.Clean(runFile), filepath.Clean(taskFile)
}

func validatePipelineLoggingConfig(config silverPipelineConfig) error {
	runPath, taskPath := config.logOutputPaths()
	if strings.TrimSpace(runPath) == "" || strings.TrimSpace(taskPath) == "" {
		return fmt.Errorf("pipeline logging paths must not be empty")
	}
	if runPath == taskPath {
		return fmt.Errorf("pipeline logging requires separate files: run=%s task=%s", runPath, taskPath)
	}
	return nil
}

func cloneLogFields(fields map[string]any) map[string]any {
	if len(fields) == 0 {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(fields))
	for key, value := range fields {
		cloned[key] = value
	}
	return cloned
}

func newSilverPipelineFileLogger(config silverPipelineConfig, tenantID string, mode string) (*silverPipelineFileLogger, error) {
	runPath, taskPath := config.logOutputPaths()

	if err := os.MkdirAll(filepath.Dir(runPath), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create silver run log directory for %s: %w", runPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create silver task log directory for %s: %w", taskPath, err)
	}

	runFile, err := os.OpenFile(runPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open silver run log file %s: %w", runPath, err)
	}

	taskFile, err := os.OpenFile(taskPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		_ = runFile.Close()
		return nil, fmt.Errorf("failed to open silver task log file %s: %w", taskPath, err)
	}

	return &silverPipelineFileLogger{
		runID:          time.Now().UTC().Format("20060102_150405.000000000"),
		tenantID:       tenantID,
		mode:           mode,
		storageBackend: config.Pipeline.StorageBackend,
		storageTarget:  storageTargetForLog(config),
		runLogFile:     runFile,
		taskLogFile:    taskFile,
	}, nil
}

func (logger *silverPipelineFileLogger) Close() {
	if logger == nil {
		return
	}
	if logger.runLogFile != nil {
		_ = logger.runLogFile.Close()
	}
	if logger.taskLogFile != nil {
		_ = logger.taskLogFile.Close()
	}
}

func (logger *silverPipelineFileLogger) writeJSONLine(target *os.File, payload map[string]any) {
	if logger == nil || target == nil {
		return
	}

	record := cloneLogFields(payload)
	record["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	record["run_id"] = logger.runID
	record["tenant_id"] = logger.tenantID
	record["mode"] = logger.mode
	record["storage_backend"] = logger.storageBackend
	record["storage_target"] = logger.storageTarget

	line, err := json.Marshal(record)
	if err != nil {
		log.Printf("Silver pipeline log marshal failed: %v", err)
		return
	}

	if _, err := target.Write(append(line, '\n')); err != nil {
		log.Printf("Silver pipeline log write failed: %v", err)
	}
}

func (logger *silverPipelineFileLogger) logRunEvent(status string, duration time.Duration, details map[string]any, runErr error) {
	entry := cloneLogFields(details)
	entry["event"] = "pipeline_run"
	entry["status"] = status
	entry["duration_ms"] = duration.Milliseconds()
	if runErr != nil {
		entry["error"] = runErr.Error()
	}
	logger.writeJSONLine(logger.runLogFile, entry)
}

func (logger *silverPipelineFileLogger) logTaskEvent(task string, status string, duration time.Duration, details map[string]any, taskErr error) {
	entry := cloneLogFields(details)
	entry["event"] = "pipeline_task"
	entry["task"] = task
	entry["status"] = status
	entry["duration_ms"] = duration.Milliseconds()
	if taskErr != nil {
		entry["error"] = taskErr.Error()
	}
	logger.writeJSONLine(logger.taskLogFile, entry)
}

func logSilverPipelineTask(task string, status string, startedAt time.Time, details map[string]any, taskErr error) {
	if activeSilverPipelineLogger == nil {
		return
	}
	activeSilverPipelineLogger.logTaskEvent(task, status, time.Since(startedAt), details, taskErr)
}

func storageAssetSize(ctx context.Context, gcsClient *storage.Client, targetPath string) (int64, error) {
	trimmedTarget := strings.TrimSpace(targetPath)
	if trimmedTarget == "" {
		return 0, fmt.Errorf("missing storage target path")
	}

	if strings.HasPrefix(trimmedTarget, "gs://") {
		if gcsClient == nil {
			return 0, fmt.Errorf("gcs storage target requires initialized client")
		}

		location, err := parseGCSURI(trimmedTarget)
		if err != nil {
			return 0, err
		}

		attrs, err := gcsClient.Bucket(location.Bucket).Object(location.Object).Attrs(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to read object attrs for %s: %w", trimmedTarget, err)
		}

		return attrs.Size, nil
	}

	info, err := os.Stat(trimmedTarget)
	if err != nil {
		return 0, fmt.Errorf("failed to stat %s: %w", trimmedTarget, err)
	}
	if info.IsDir() {
		return 0, fmt.Errorf("storage target %s is a directory", trimmedTarget)
	}

	return info.Size(), nil
}

func (config silverPipelineConfig) maxRuntime() time.Duration {
	if config.PipelineConstraints.Scheduling.MaxPipelineRuntimeSec <= 0 {
		return defaultSilverPipelineRuntime
	}
	return time.Duration(config.PipelineConstraints.Scheduling.MaxPipelineRuntimeSec) * time.Second
}

func (config silverPipelineConfig) maxRetries() int {
	if config.PipelineConstraints.Reliability.MaxRetries <= 0 {
		return defaultSilverPipelineMaxRetries
	}
	return config.PipelineConstraints.Reliability.MaxRetries
}

func (config silverPipelineConfig) dropRowsWithMissingEntries() bool {
	if config.Pipeline.Transformation.DropRowsWithMissingEntries == nil {
		return true
	}
	return *config.Pipeline.Transformation.DropRowsWithMissingEntries
}

func cassandraConsistencyForSilverPipeline(tier string) gocql.Consistency {
	switch normalizeTenantTier(tier) {
	case tenantTierSilver:
		return gocql.One
	default:
		return gocql.Quorum
	}
}

func cassandraConsistencyName(consistency gocql.Consistency) string {
	switch consistency {
	case gocql.One:
		return "ONE"
	case gocql.Quorum:
		return "QUORUM"
	case gocql.All:
		return "ALL"
	default:
		return fmt.Sprintf("%d", int(consistency))
	}
}

func parsePositiveIntEnv(key string, defaultValue int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultValue
	}

	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 {
		log.Printf("Invalid %s=%q, using default=%d", key, raw, defaultValue)
		return defaultValue
	}

	return parsed
}

func normalizeExtractDay(value string) (string, error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return "", fmt.Errorf("missing extract day: set pipeline.extract_day or %s", silverPipelineDayEnv)
	}

	if _, err := time.Parse("2006-01-02", normalized); err != nil {
		return "", fmt.Errorf("invalid extract day %q: expected YYYY-MM-DD", normalized)
	}

	return normalized, nil
}

func normalizeSchemaToken(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	normalized = strings.ReplaceAll(normalized, " ", "_")
	return normalized
}

func sanitizeIdentifierPart(input string, fallback string) string {
	trimmed := strings.ToLower(strings.TrimSpace(input))
	if trimmed == "" {
		return fallback
	}

	var builder strings.Builder
	previousUnderscore := false

	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
			previousUnderscore = false
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
			previousUnderscore = false
		case r == '_' || r == '-' || r == ' ' || r == '.':
			if !previousUnderscore {
				builder.WriteRune('_')
				previousUnderscore = true
			}
		}
	}

	output := strings.Trim(builder.String(), "_")
	if output == "" {
		return fallback
	}

	return output
}

func listBronzeTables() ([]string, error) {
	return []string{hardcodedTenant2BronzeTable}, nil
}

func bronzeTableSuffix(tableName string) (string, error) {
	if tableName != hardcodedTenant2BronzeTable {
		return "", fmt.Errorf("unexpected bronze table: got=%s want=%s", tableName, hardcodedTenant2BronzeTable)
	}
	return hardcodedTenant2TableSuffix, nil
}

func silverTableName() string {
	return fmt.Sprintf("%s.%s", cassandraKeyspace, hardcodedTenant2SilverTable)
}

func bronzeExtractCachePath(cacheDir string, tableName string, runID string) string {
	baseName := fmt.Sprintf("%s_%s_bronze_extract.csv", tableName, runID)
	return filepath.Join(cacheDir, baseName)
}

func bronzeExtractObjectPath(prefix string, tableName string, runID string) string {
	baseName := fmt.Sprintf("%s_%s_bronze_extract.csv", tableName, runID)
	if normalizedPrefix := normalizeGCSPrefix(prefix); normalizedPrefix != "" {
		return path.Join(normalizedPrefix, baseName)
	}
	return baseName
}

func silverSummaryCachePath(cacheDir string, tableName string, runID string) string {
	baseName := fmt.Sprintf("%s_%s_%s.csv", tableName, runID, defaultSilverSummarySuffix)
	return filepath.Join(cacheDir, baseName)
}

func gcsURI(bucket string, object string) string {
	trimmedBucket := strings.TrimSpace(bucket)
	trimmedObject := strings.TrimLeft(strings.TrimSpace(object), "/")
	if trimmedObject == "" {
		return fmt.Sprintf("gs://%s", trimmedBucket)
	}
	return fmt.Sprintf("gs://%s/%s", trimmedBucket, trimmedObject)
}

type gcsObjectLocation struct {
	Bucket string
	Object string
}

func parseGCSURI(uri string) (gcsObjectLocation, error) {
	normalized := strings.TrimSpace(uri)
	if !strings.HasPrefix(normalized, "gs://") {
		return gcsObjectLocation{}, fmt.Errorf("invalid GCS path %q: expected gs://<bucket>/<object>", uri)
	}

	rawPath := strings.TrimPrefix(normalized, "gs://")
	parts := strings.SplitN(rawPath, "/", 2)
	bucket := strings.TrimSpace(parts[0])
	if bucket == "" {
		return gcsObjectLocation{}, fmt.Errorf("invalid GCS path %q: missing bucket", uri)
	}

	if len(parts) < 2 {
		return gcsObjectLocation{}, fmt.Errorf("invalid GCS path %q: missing object name", uri)
	}

	object := strings.Trim(strings.TrimSpace(parts[1]), "/")
	if object == "" {
		return gcsObjectLocation{}, fmt.Errorf("invalid GCS path %q: missing object name", uri)
	}

	return gcsObjectLocation{Bucket: bucket, Object: object}, nil
}

func silverSummaryObjectPathFromBronzeObject(bronzeObject string) string {
	return strings.TrimSuffix(bronzeObject, "_bronze_extract.csv") + "_" + defaultSilverSummarySuffix + ".csv"
}

func discoverCachedBronzeFilesLocal(cacheDir string, inputGlob string) ([]string, error) {
	pattern := strings.TrimSpace(inputGlob)
	if pattern == "" {
		pattern = defaultBatchmanagerInputGlob
	}

	matches, err := filepath.Glob(filepath.Join(cacheDir, pattern))
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate cache glob %s: %w", pattern, err)
	}

	sort.Strings(matches)
	return matches, nil
}

func gcsObjectMatchesPattern(objectName string, prefix string, pattern string) bool {
	relativeName := objectName
	normalizedPrefix := normalizeGCSPrefix(prefix)
	if normalizedPrefix != "" {
		prefixWithSlash := normalizedPrefix + "/"
		relativeName = strings.TrimPrefix(objectName, prefixWithSlash)
	}

	target := path.Base(relativeName)
	if strings.Contains(pattern, "/") {
		target = relativeName
	}

	matched, err := path.Match(pattern, target)
	if err != nil {
		return false
	}
	return matched
}

func discoverCachedBronzeFilesGCS(ctx context.Context, gcsClient *storage.Client, bucket string, prefix string, inputGlob string) ([]string, error) {
	pattern := strings.TrimSpace(inputGlob)
	if pattern == "" {
		pattern = defaultBatchmanagerInputGlob
	}

	query := &storage.Query{}
	normalizedPrefix := normalizeGCSPrefix(prefix)
	if normalizedPrefix != "" {
		query.Prefix = normalizedPrefix + "/"
	}

	iter := gcsClient.Bucket(bucket).Objects(ctx, query)
	matched := make([]string, 0)
	for {
		attrs, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list cached bronze files in gs://%s/%s: %w", bucket, normalizedPrefix, err)
		}
		if attrs == nil || strings.HasSuffix(attrs.Name, "/") {
			continue
		}

		if !gcsObjectMatchesPattern(attrs.Name, normalizedPrefix, pattern) {
			continue
		}

		matched = append(matched, gcsURI(bucket, attrs.Name))
	}

	sort.Strings(matched)
	return matched, nil
}

func resolveTransformInputFilesLocal(cacheDir string, inputGlob string) ([]string, error) {
	rawInputs := strings.TrimSpace(os.Getenv(silverPipelineInputFilesEnv))
	if rawInputs == "" {
		return discoverCachedBronzeFilesLocal(cacheDir, inputGlob)
	}

	resolved := make([]string, 0)
	seen := make(map[string]struct{})
	for _, part := range strings.Split(rawInputs, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}

		candidate := trimmed
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(cacheDir, trimmed)
		}

		candidate = filepath.Clean(candidate)
		if _, exists := seen[candidate]; exists {
			continue
		}

		if _, err := os.Stat(candidate); err != nil {
			return nil, fmt.Errorf("transform-cache input file not found: %s", candidate)
		}

		resolved = append(resolved, candidate)
		seen[candidate] = struct{}{}
	}

	sort.Strings(resolved)
	return resolved, nil
}

func resolveGCSObjectName(prefix string, input string) string {
	trimmed := strings.Trim(strings.TrimSpace(input), "/")
	if trimmed == "" {
		return ""
	}

	normalizedPrefix := normalizeGCSPrefix(prefix)
	if normalizedPrefix == "" {
		return trimmed
	}

	if strings.HasPrefix(trimmed, normalizedPrefix+"/") {
		return trimmed
	}

	return path.Join(normalizedPrefix, trimmed)
}

func resolveTransformInputFilesGCS(ctx context.Context, gcsClient *storage.Client, bucket string, prefix string, inputGlob string) ([]string, error) {
	rawInputs := strings.TrimSpace(os.Getenv(silverPipelineInputFilesEnv))
	if rawInputs == "" {
		return discoverCachedBronzeFilesGCS(ctx, gcsClient, bucket, prefix, inputGlob)
	}

	resolved := make([]string, 0)
	seen := make(map[string]struct{})
	for _, part := range strings.Split(rawInputs, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}

		var location gcsObjectLocation
		if strings.HasPrefix(trimmed, "gs://") {
			parsed, err := parseGCSURI(trimmed)
			if err != nil {
				return nil, err
			}
			if parsed.Bucket != bucket {
				return nil, fmt.Errorf("transform-cache input %s is in bucket %s, expected bucket %s", trimmed, parsed.Bucket, bucket)
			}
			location = parsed
		} else {
			objectName := resolveGCSObjectName(prefix, trimmed)
			if objectName == "" {
				continue
			}
			location = gcsObjectLocation{Bucket: bucket, Object: objectName}
		}

		canonicalPath := gcsURI(location.Bucket, location.Object)
		if _, exists := seen[canonicalPath]; exists {
			continue
		}

		if _, err := gcsClient.Bucket(location.Bucket).Object(location.Object).Attrs(ctx); err != nil {
			return nil, fmt.Errorf("transform-cache input file not found: %s", canonicalPath)
		}

		resolved = append(resolved, canonicalPath)
		seen[canonicalPath] = struct{}{}
	}

	sort.Strings(resolved)
	return resolved, nil
}

func resolveTransformInputFiles(ctx context.Context, pipelineConfig silverPipelineConfig, gcsClient *storage.Client) ([]string, error) {
	switch pipelineConfig.Pipeline.StorageBackend {
	case silverPipelineStorageLocal:
		return resolveTransformInputFilesLocal(pipelineConfig.Pipeline.CacheDir, pipelineConfig.Pipeline.BatchManager.InputGlob)
	case silverPipelineStorageGCS:
		if gcsClient == nil {
			return nil, fmt.Errorf("gcs storage backend requires initialized client")
		}
		return resolveTransformInputFilesGCS(
			ctx,
			gcsClient,
			strings.TrimSpace(pipelineConfig.Pipeline.GCS.Bucket),
			pipelineConfig.Pipeline.GCS.Prefix,
			pipelineConfig.Pipeline.BatchManager.InputGlob,
		)
	default:
		return nil, fmt.Errorf("unsupported storage backend %q", pipelineConfig.Pipeline.StorageBackend)
	}
}

func looksLikeRunID(value string) bool {
	if len(value) != len("20060102_150405") {
		return false
	}

	for idx, r := range value {
		switch {
		case idx == 8:
			if r != '_' {
				return false
			}
		case r < '0' || r > '9':
			return false
		}
	}

	return true
}

func bronzeTableNameFromCacheFile(cacheFile string) (string, error) {
	baseName := filepath.Base(cacheFile)
	if !strings.HasSuffix(baseName, "_bronze_extract.csv") {
		return "", fmt.Errorf("cache file %s does not match bronze extract naming", cacheFile)
	}

	trimmed := strings.TrimSuffix(baseName, "_bronze_extract.csv")
	if len(trimmed) <= len("_20060102_150405") {
		return "", fmt.Errorf("cache file %s is missing run id", cacheFile)
	}

	runIDStart := len(trimmed) - len("20060102_150405")
	runID := trimmed[runIDStart:]
	if !looksLikeRunID(runID) {
		return "", fmt.Errorf("cache file %s has invalid run id suffix", cacheFile)
	}

	separatorIndex := runIDStart - 1
	if separatorIndex < 0 || trimmed[separatorIndex] != '_' {
		return "", fmt.Errorf("cache file %s has invalid bronze extract separator", cacheFile)
	}

	bronzeTableName := trimmed[:separatorIndex]
	if bronzeTableName == "" {
		return "", fmt.Errorf("cache file %s is missing bronze table name", cacheFile)
	}

	return bronzeTableName, nil
}

func silverSummaryPathFromCacheFile(cacheFile string) string {
	return strings.TrimSuffix(cacheFile, "_bronze_extract.csv") + "_" + defaultSilverSummarySuffix + ".csv"
}

func extractBronzeTableToWriter(ctx context.Context, session *gocql.Session, tenantConfig TenantConfig, tableName string, output io.Writer, outputName string, extractDay string, pageSize int) (int, error) {
	writer := csv.NewWriter(output)
	writer.Comma = ';'

	columnNames := make([]string, 0, len(tenantConfig.Schema.Columns))
	for _, column := range tenantConfig.Schema.Columns {
		columnNames = append(columnNames, normalizeSchemaToken(column.Name))
	}

	if err := writer.Write(columnNames); err != nil {
		return 0, fmt.Errorf("failed to write bronze cache header to %s: %w", outputName, err)
	}

	selectQuery := fmt.Sprintf(
		"SELECT %s FROM %s.%s WHERE day = ? AND hour = ?",
		strings.Join(columnNames, ", "),
		cassandraKeyspace,
		tableName,
	)

	rowCount := 0
	for hour := 0; hour < 24; hour++ {
		iter := session.Query(selectQuery, extractDay, hour).PageSize(pageSize).WithContext(ctx).Iter()
		row := map[string]any{}
		for iter.MapScan(row) {
			record := make([]string, 0, len(columnNames))
			for _, columnName := range columnNames {
				record = append(record, formatCassandraValue(row[columnName]))
			}

			if err := writer.Write(record); err != nil {
				return rowCount, fmt.Errorf("failed to write bronze cache row for %s: %w", tableName, err)
			}

			rowCount++
			row = map[string]any{}
		}

		if err := iter.Close(); err != nil {
			return rowCount, fmt.Errorf("failed to extract bronze rows from %s for day=%s hour=%d: %w", tableName, extractDay, hour, err)
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return rowCount, fmt.Errorf("failed to flush bronze cache stream %s: %w", outputName, err)
	}

	return rowCount, nil
}

func extractBronzeTableToCSV(ctx context.Context, session *gocql.Session, tenantConfig TenantConfig, tableName string, outputPath string, extractDay string, pageSize int) (int, error) {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return 0, fmt.Errorf("failed to create cache directory for %s: %w", outputPath, err)
	}

	file, err := os.Create(outputPath)
	if err != nil {
		return 0, fmt.Errorf("failed to create bronze cache file %s: %w", outputPath, err)
	}
	defer file.Close()

	return extractBronzeTableToWriter(ctx, session, tenantConfig, tableName, file, outputPath, extractDay, pageSize)
}

func extractBronzeTableToGCS(ctx context.Context, gcsClient *storage.Client, bucket string, objectPath string, session *gocql.Session, tenantConfig TenantConfig, tableName string, extractDay string, pageSize int) (int, error) {
	writer := gcsClient.Bucket(bucket).Object(objectPath).NewWriter(ctx)
	writer.ContentType = "text/csv"

	outputName := gcsURI(bucket, objectPath)
	rowCount, err := extractBronzeTableToWriter(ctx, session, tenantConfig, tableName, writer, outputName, extractDay, pageSize)
	if err != nil {
		_ = writer.Close()
		return rowCount, err
	}

	if err := writer.Close(); err != nil {
		return rowCount, fmt.Errorf("failed to finalize bronze cache object %s: %w", outputName, err)
	}

	return rowCount, nil
}

func formatCassandraValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	case float32:
		return strconv.FormatFloat(float64(typed), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case bool:
		return strconv.FormatBool(typed)
	case time.Time:
		return typed.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func aggregateHourlyCSV(csvPath string, metricFields []string, dropRowsWithMissingEntries bool) ([]silverAggregateRow, int, int, error) {
	file, err := os.Open(csvPath)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to open cached bronze csv %s: %w", csvPath, err)
	}
	defer file.Close()

	return aggregateHourlyCSVFromReader(file, csvPath, metricFields, dropRowsWithMissingEntries)
}

func aggregateHourlyGCSObject(ctx context.Context, gcsClient *storage.Client, objectURI string, metricFields []string, dropRowsWithMissingEntries bool) ([]silverAggregateRow, int, int, error) {
	location, err := parseGCSURI(objectURI)
	if err != nil {
		return nil, 0, 0, err
	}

	reader, err := gcsClient.Bucket(location.Bucket).Object(location.Object).NewReader(ctx)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to open cached bronze object %s: %w", objectURI, err)
	}
	defer reader.Close()

	return aggregateHourlyCSVFromReader(reader, objectURI, metricFields, dropRowsWithMissingEntries)
}

func aggregateHourlyCSVFromReader(input io.Reader, sourceName string, metricFields []string, dropRowsWithMissingEntries bool) ([]silverAggregateRow, int, int, error) {
	reader := csv.NewReader(input)
	reader.Comma = ';'
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to read cached bronze csv header %s: %w", sourceName, err)
	}

	columnIndexes := make(map[string]int, len(header))
	for idx, rawColumn := range header {
		columnIndexes[normalizeSchemaToken(rawColumn)] = idx
	}

	requiredColumns := append([]string{"day", "hour"}, metricFields...)
	for _, column := range requiredColumns {
		if _, exists := columnIndexes[column]; !exists {
			return nil, 0, 0, fmt.Errorf("cached bronze csv %s is missing required column %s", sourceName, column)
		}
	}

	groups := make(map[hourlyGroupKey]*hourlyAccumulator)
	droppedRows := 0
	keptRows := 0

	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, droppedRows, keptRows, fmt.Errorf("failed to read cached bronze csv row from %s: %w", sourceName, err)
		}

		if dropRowsWithMissingEntries && recordHasMissingEntries(record, len(header)) {
			droppedRows++
			continue
		}

		day := strings.TrimSpace(record[columnIndexes["day"]])
		hourValue := strings.TrimSpace(record[columnIndexes["hour"]])
		if day == "" || hourValue == "" {
			droppedRows++
			continue
		}

		hour, err := strconv.Atoi(hourValue)
		if err != nil {
			droppedRows++
			continue
		}

		parsedValues := make(map[string]float64, len(metricFields))
		parseFailed := false
		for _, field := range metricFields {
			rawValue := strings.TrimSpace(record[columnIndexes[field]])
			if rawValue == "" {
				parseFailed = true
				break
			}

			parsedValue, err := strconv.ParseFloat(rawValue, 64)
			if err != nil {
				parseFailed = true
				break
			}

			parsedValues[field] = parsedValue
		}

		if parseFailed {
			droppedRows++
			continue
		}

		key := hourlyGroupKey{Day: day, Hour: hour}
		accumulator, exists := groups[key]
		if !exists {
			accumulator = &hourlyAccumulator{Metrics: make(map[string]*metricAccumulator, len(metricFields))}
			for _, field := range metricFields {
				accumulator.Metrics[field] = &metricAccumulator{}
			}
			groups[key] = accumulator
		}

		accumulator.RecordCount++
		for _, field := range metricFields {
			accumulator.Metrics[field].Add(parsedValues[field])
		}

		keptRows++
	}

	aggregates := make([]silverAggregateRow, 0, len(groups))
	keys := make([]hourlyGroupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Day == keys[j].Day {
			return keys[i].Hour < keys[j].Hour
		}
		return keys[i].Day < keys[j].Day
	})

	for _, key := range keys {
		accumulator := groups[key]
		row := silverAggregateRow{
			Day:               key.Day,
			Hour:              key.Hour,
			RecordsAggregated: accumulator.RecordCount,
			Metrics:           make(map[string]metricSummary, len(metricFields)),
		}

		for _, field := range metricFields {
			row.Metrics[field] = accumulator.Metrics[field].Summary()
		}

		aggregates = append(aggregates, row)
	}

	return aggregates, droppedRows, keptRows, nil
}

func recordHasMissingEntries(record []string, expectedColumns int) bool {
	if len(record) < expectedColumns {
		return true
	}

	for idx := 0; idx < expectedColumns; idx++ {
		if strings.TrimSpace(record[idx]) == "" {
			return true
		}
	}

	return false
}

func (accumulator *metricAccumulator) Add(value float64) {
	if len(accumulator.Values) == 0 {
		accumulator.Min = value
		accumulator.Max = value
	} else {
		if value < accumulator.Min {
			accumulator.Min = value
		}
		if value > accumulator.Max {
			accumulator.Max = value
		}
	}

	accumulator.Values = append(accumulator.Values, value)
	accumulator.Sum += value
}

func (accumulator *metricAccumulator) Summary() metricSummary {
	if len(accumulator.Values) == 0 {
		return metricSummary{}
	}

	sort.Float64s(accumulator.Values)
	median := 0.0
	middle := len(accumulator.Values) / 2
	if len(accumulator.Values)%2 == 0 {
		median = (accumulator.Values[middle-1] + accumulator.Values[middle]) / 2
	} else {
		median = accumulator.Values[middle]
	}

	return metricSummary{
		Avg:    accumulator.Sum / float64(len(accumulator.Values)),
		Min:    accumulator.Min,
		Max:    accumulator.Max,
		Median: median,
	}
}

func writeSilverSummaryCSV(outputPath string, aggregates []silverAggregateRow, metricFields []string) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("failed to create cache directory for %s: %w", outputPath, err)
	}

	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create silver summary csv %s: %w", outputPath, err)
	}
	defer file.Close()

	return writeSilverSummaryCSVToWriter(file, outputPath, aggregates, metricFields)
}

func writeSilverSummaryGCSObject(ctx context.Context, gcsClient *storage.Client, bronzeObjectURI string, aggregates []silverAggregateRow, metricFields []string) (string, error) {
	location, err := parseGCSURI(bronzeObjectURI)
	if err != nil {
		return "", err
	}

	summaryObject := silverSummaryObjectPathFromBronzeObject(location.Object)
	summaryURI := gcsURI(location.Bucket, summaryObject)

	var buffer bytes.Buffer
	if err := writeSilverSummaryCSVToWriter(&buffer, summaryURI, aggregates, metricFields); err != nil {
		return "", err
	}

	writer := gcsClient.Bucket(location.Bucket).Object(summaryObject).NewWriter(ctx)
	writer.ContentType = "text/csv"
	if _, err := io.Copy(writer, &buffer); err != nil {
		_ = writer.Close()
		return "", fmt.Errorf("failed to write silver summary object %s: %w", summaryURI, err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("failed to finalize silver summary object %s: %w", summaryURI, err)
	}

	return summaryURI, nil
}

func writeSilverSummaryCSVToWriter(output io.Writer, outputName string, aggregates []silverAggregateRow, metricFields []string) error {
	writer := csv.NewWriter(output)
	writer.Comma = ';'

	header := []string{"day", "hour", "records_aggregated"}
	for _, field := range metricFields {
		header = append(
			header,
			field+"_avg",
			field+"_min",
			field+"_max",
			field+"_median",
		)
	}

	if err := writer.Write(header); err != nil {
		return fmt.Errorf("failed to write silver summary header to %s: %w", outputName, err)
	}

	for _, row := range aggregates {
		record := []string{row.Day, strconv.Itoa(row.Hour), strconv.Itoa(row.RecordsAggregated)}
		for _, field := range metricFields {
			summary := row.Metrics[field]
			record = append(
				record,
				formatFloat(summary.Avg),
				formatFloat(summary.Min),
				formatFloat(summary.Max),
				formatFloat(summary.Median),
			)
		}

		if err := writer.Write(record); err != nil {
			return fmt.Errorf("failed to write silver summary row to %s: %w", outputName, err)
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("failed to flush silver summary csv %s: %w", outputName, err)
	}

	return nil
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func createSilverTableCQL(tableName string, metricFields []string) string {
	columnDefs := []string{
		"day text",
		"hour int",
		"records_aggregated int",
	}

	for _, field := range metricFields {
		columnName := sanitizeIdentifierPart(field, field)
		columnDefs = append(
			columnDefs,
			fmt.Sprintf("%s_avg double", columnName),
			fmt.Sprintf("%s_min double", columnName),
			fmt.Sprintf("%s_max double", columnName),
			fmt.Sprintf("%s_median double", columnName),
		)
	}

	columnDefs = append(columnDefs, "PRIMARY KEY ((day), hour)")
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)", tableName, strings.Join(columnDefs, ", "))
}

func insertSilverCQL(tableName string, metricFields []string) string {
	columnNames := []string{"day", "hour", "records_aggregated"}
	placeholders := []string{"?", "?", "?"}

	for _, field := range metricFields {
		columnName := sanitizeIdentifierPart(field, field)
		columnNames = append(
			columnNames,
			columnName+"_avg",
			columnName+"_min",
			columnName+"_max",
			columnName+"_median",
		)
		placeholders = append(placeholders, "?", "?", "?", "?")
	}

	return fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		tableName,
		strings.Join(columnNames, ", "),
		strings.Join(placeholders, ", "),
	)
}

func createSilverTableIfNotExists(ctx context.Context, session *gocql.Session, tableName string, metricFields []string, maxRetries int) error {
	createQuery := createSilverTableCQL(tableName, metricFields)
	if err := execCQLWithRetry(ctx, session, maxRetries, createQuery); err != nil {
		return fmt.Errorf("failed to create silver table %s: %w", tableName, err)
	}
	return nil
}

func insertSilverAggregates(ctx context.Context, session *gocql.Session, tableName string, aggregates []silverAggregateRow, metricFields []string, maxRetries int) (int, error) {
	insertQuery := insertSilverCQL(tableName, metricFields)
	insertedRows := 0

	for _, row := range aggregates {
		args := make([]any, 0, 3+(4*len(metricFields)))
		args = append(args, row.Day, row.Hour, row.RecordsAggregated)
		for _, field := range metricFields {
			summary := row.Metrics[field]
			args = append(args, summary.Avg, summary.Min, summary.Max, summary.Median)
		}

		if err := execCQLWithRetry(ctx, session, maxRetries, insertQuery, args...); err != nil {
			return insertedRows, fmt.Errorf("failed to insert silver aggregate into %s for day=%s hour=%d: %w", tableName, row.Day, row.Hour, err)
		}

		insertedRows++
	}

	return insertedRows, nil
}

func execCQLWithRetry(ctx context.Context, session *gocql.Session, maxRetries int, query string, args ...any) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := session.Query(query, args...).WithContext(ctx).Exec()
		if err == nil {
			return nil
		}

		lastErr = err
		if attempt == maxRetries {
			break
		}

		delay := retryBackoff(attempt)
		log.Printf(
			"Silver pipeline CQL retry attempt=%d/%d err=%v backoff=%s",
			attempt+1,
			maxRetries+1,
			lastErr,
			delay,
		)

		if err := sleepWithContext(ctx, delay); err != nil {
			return err
		}
	}

	return lastErr
}

func retryBackoff(attempt int) time.Duration {
	delay := defaultSilverPipelineRetryBackoff
	for i := 0; i < attempt; i++ {
		if delay >= maxSilverPipelineRetryBackoff/2 {
			return maxSilverPipelineRetryBackoff
		}
		delay *= 2
	}
	if delay > maxSilverPipelineRetryBackoff {
		return maxSilverPipelineRetryBackoff
	}
	return delay
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newGCSClient(ctx context.Context, pipelineConfig silverPipelineConfig) (*storage.Client, error) {
	clientOptions := make([]option.ClientOption, 0)
	if credsFile := strings.TrimSpace(pipelineConfig.Pipeline.GCS.CredentialsFile); credsFile != "" {
		clientOptions = append(clientOptions, option.WithCredentialsFile(credsFile))
	}

	client, err := storage.NewClient(ctx, clientOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize GCS client: %w", err)
	}

	return client, nil
}

func directorySize(path string) (int64, error) {
	var totalSize int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info == nil || info.IsDir() {
			return nil
		}
		totalSize += info.Size()
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return totalSize, nil
}

func enforceCacheSizeLimit(cacheDir string, maxSilverStorageGB int) error {
	if maxSilverStorageGB <= 0 {
		return nil
	}

	currentSize, err := directorySize(cacheDir)
	if err != nil {
		return fmt.Errorf("failed to measure tenant cache directory %s: %w", cacheDir, err)
	}

	maxBytes := int64(maxSilverStorageGB) * 1024 * 1024 * 1024
	if currentSize > maxBytes {
		return fmt.Errorf("tenant cache directory %s exceeds configured storage limit: size=%d limit=%d", cacheDir, currentSize, maxBytes)
	}

	return nil
}

func gcsPrefixSize(ctx context.Context, gcsClient *storage.Client, bucket string, prefix string) (int64, error) {
	query := &storage.Query{}
	normalizedPrefix := normalizeGCSPrefix(prefix)
	if normalizedPrefix != "" {
		query.Prefix = normalizedPrefix + "/"
	}

	iter := gcsClient.Bucket(bucket).Objects(ctx, query)
	var totalSize int64
	for {
		attrs, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("failed to list objects in gs://%s/%s: %w", bucket, normalizedPrefix, err)
		}
		if attrs == nil || strings.HasSuffix(attrs.Name, "/") {
			continue
		}
		totalSize += attrs.Size
	}

	return totalSize, nil
}

func enforceGCSSizeLimit(ctx context.Context, gcsClient *storage.Client, bucket string, prefix string, maxSilverStorageGB int) error {
	if maxSilverStorageGB <= 0 {
		return nil
	}

	currentSize, err := gcsPrefixSize(ctx, gcsClient, bucket, prefix)
	if err != nil {
		return err
	}

	maxBytes := int64(maxSilverStorageGB) * 1024 * 1024 * 1024
	if currentSize > maxBytes {
		target := gcsURI(bucket, normalizeGCSPrefix(prefix))
		return fmt.Errorf("gcs cache prefix %s exceeds configured storage limit: size=%d limit=%d", target, currentSize, maxBytes)
	}

	return nil
}

func enforceStorageSizeLimit(ctx context.Context, pipelineConfig silverPipelineConfig, gcsClient *storage.Client) error {
	switch pipelineConfig.Pipeline.StorageBackend {
	case silverPipelineStorageLocal:
		return enforceCacheSizeLimit(pipelineConfig.Pipeline.CacheDir, pipelineConfig.PipelineConstraints.Storage.MaxSilverStorageGB)
	case silverPipelineStorageGCS:
		if gcsClient == nil {
			return fmt.Errorf("gcs storage backend requires initialized client")
		}
		return enforceGCSSizeLimit(
			ctx,
			gcsClient,
			strings.TrimSpace(pipelineConfig.Pipeline.GCS.Bucket),
			pipelineConfig.Pipeline.GCS.Prefix,
			pipelineConfig.PipelineConstraints.Storage.MaxSilverStorageGB,
		)
	default:
		return fmt.Errorf("unsupported storage backend %q", pipelineConfig.Pipeline.StorageBackend)
	}
}

func enforceStorageSizeLimitWithTaskLog(ctx context.Context, pipelineConfig silverPipelineConfig, gcsClient *storage.Client, scope string) error {
	startedAt := time.Now()
	maxStorageGB := pipelineConfig.PipelineConstraints.Storage.MaxSilverStorageGB
	details := map[string]any{
		"scope":                 scope,
		"max_silver_storage_gb": maxStorageGB,
	}

	err := enforceStorageSizeLimit(ctx, pipelineConfig, gcsClient)
	if err != nil {
		logSilverPipelineTask("validate_storage_limit", "failed", startedAt, details, err)
		return err
	}

	logSilverPipelineTask("validate_storage_limit", "success", startedAt, details, nil)
	return nil
}

func extractBronzeTablesToCache(ctx context.Context, session *gocql.Session, tenantConfig TenantConfig, pipelineConfig silverPipelineConfig, gcsClient *storage.Client) ([]string, error) {
	if pipelineConfig.Pipeline.StorageBackend == silverPipelineStorageLocal {
		if err := os.MkdirAll(pipelineConfig.Pipeline.CacheDir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create tenant cache directory %s: %w", pipelineConfig.Pipeline.CacheDir, err)
		}
	} else if pipelineConfig.Pipeline.StorageBackend == silverPipelineStorageGCS && gcsClient == nil {
		return nil, fmt.Errorf("gcs storage backend requires initialized client")
	}

	extractDay, err := normalizeExtractDay(pipelineConfig.Pipeline.ExtractDay)
	if err != nil {
		return nil, err
	}

	bronzeTables, err := listBronzeTables()
	if err != nil {
		return nil, err
	}

	runID := time.Now().UTC().Format("20060102_150405")
	extractedFiles := make([]string, 0, len(bronzeTables))
	for _, bronzeTable := range bronzeTables {
		taskStartedAt := time.Now()
		taskDetails := map[string]any{
			"bronze_table": bronzeTable,
			"extract_day":  extractDay,
		}

		bronzeCachePath := ""
		rawRows := 0

		switch pipelineConfig.Pipeline.StorageBackend {
		case silverPipelineStorageLocal:
			bronzeCachePath = bronzeExtractCachePath(pipelineConfig.Pipeline.CacheDir, bronzeTable, runID)
			rawRows, err = extractBronzeTableToCSV(ctx, session, tenantConfig, bronzeTable, bronzeCachePath, extractDay, pipelineConfig.Pipeline.ExtractPageSize)
		case silverPipelineStorageGCS:
			bucket := strings.TrimSpace(pipelineConfig.Pipeline.GCS.Bucket)
			bronzeObjectPath := bronzeExtractObjectPath(pipelineConfig.Pipeline.GCS.Prefix, bronzeTable, runID)
			rawRows, err = extractBronzeTableToGCS(ctx, gcsClient, bucket, bronzeObjectPath, session, tenantConfig, bronzeTable, extractDay, pipelineConfig.Pipeline.ExtractPageSize)
			bronzeCachePath = gcsURI(bucket, bronzeObjectPath)
		default:
			return nil, fmt.Errorf("unsupported storage backend %q", pipelineConfig.Pipeline.StorageBackend)
		}

		if err != nil {
			logSilverPipelineTask("extract_bronze_table", "failed", taskStartedAt, taskDetails, err)
			return nil, err
		}

		taskDetails["cache_csv"] = bronzeCachePath
		taskDetails["raw_rows"] = rawRows

		sizeBytes, sizeErr := storageAssetSize(ctx, gcsClient, bronzeCachePath)
		if sizeErr != nil {
			taskDetails["cache_size_bytes"] = -1
			taskDetails["cache_size_error"] = sizeErr.Error()
		} else {
			taskDetails["cache_size_bytes"] = sizeBytes
		}

		if err := enforceStorageSizeLimitWithTaskLog(ctx, pipelineConfig, gcsClient, "post-extract:"+bronzeTable); err != nil {
			logSilverPipelineTask("extract_bronze_table", "failed", taskStartedAt, taskDetails, err)
			return nil, err
		}

		log.Printf(
			"Silver pipeline extracted bronze_table=%s day=%s raw_rows=%d cache_csv=%s",
			bronzeTable,
			extractDay,
			rawRows,
			bronzeCachePath,
		)
		logSilverPipelineTask("extract_bronze_table", "success", taskStartedAt, taskDetails, nil)

		extractedFiles = append(extractedFiles, bronzeCachePath)
	}

	return extractedFiles, nil
}

func processCachedBronzeFile(ctx context.Context, session *gocql.Session, tenantConfig TenantConfig, pipelineConfig silverPipelineConfig, gcsClient *storage.Client, cacheFile string) error {
	transformStartedAt := time.Now()
	transformDetails := map[string]any{"cache_file": cacheFile}

	bronzeTable, err := bronzeTableNameFromCacheFile(cacheFile)
	if err != nil {
		logSilverPipelineTask("transform_cache_file", "failed", transformStartedAt, transformDetails, err)
		return err
	}
	transformDetails["bronze_table"] = bronzeTable

	tableSuffix, err := bronzeTableSuffix(bronzeTable)
	if err != nil {
		logSilverPipelineTask("transform_cache_file", "failed", transformStartedAt, transformDetails, err)
		return err
	}
	transformDetails["table_suffix"] = tableSuffix

	inputSizeBytes, inputSizeErr := storageAssetSize(ctx, gcsClient, cacheFile)
	if inputSizeErr != nil {
		transformDetails["input_size_bytes"] = -1
		transformDetails["input_size_error"] = inputSizeErr.Error()
	} else {
		transformDetails["input_size_bytes"] = inputSizeBytes
	}

	aggregates := make([]silverAggregateRow, 0)
	droppedRows := 0
	keptRows := 0
	aggregateStartedAt := time.Now()

	switch pipelineConfig.Pipeline.StorageBackend {
	case silverPipelineStorageLocal:
		aggregates, droppedRows, keptRows, err = aggregateHourlyCSV(
			cacheFile,
			pipelineConfig.Pipeline.Transformation.MetricFields,
			pipelineConfig.dropRowsWithMissingEntries(),
		)
	case silverPipelineStorageGCS:
		if gcsClient == nil {
			return fmt.Errorf("gcs storage backend requires initialized client")
		}
		aggregates, droppedRows, keptRows, err = aggregateHourlyGCSObject(
			ctx,
			gcsClient,
			cacheFile,
			pipelineConfig.Pipeline.Transformation.MetricFields,
			pipelineConfig.dropRowsWithMissingEntries(),
		)
	default:
		err = fmt.Errorf("unsupported storage backend %q", pipelineConfig.Pipeline.StorageBackend)
	}

	if err != nil {
		logSilverPipelineTask("aggregate_bronze_cache", "failed", aggregateStartedAt, transformDetails, err)
		logSilverPipelineTask("transform_cache_file", "failed", transformStartedAt, transformDetails, err)
		return err
	}

	rawRows := keptRows + droppedRows
	aggregateDetails := cloneLogFields(transformDetails)
	aggregateDetails["raw_rows"] = rawRows
	aggregateDetails["kept_rows"] = keptRows
	aggregateDetails["dropped_rows"] = droppedRows
	aggregateDetails["aggregate_rows"] = len(aggregates)
	logSilverPipelineTask("aggregate_bronze_cache", "success", aggregateStartedAt, aggregateDetails, nil)

	if len(aggregates) == 0 {
		log.Printf(
			"Silver pipeline skipped cache_file=%s because no complete rows remained after cleaning (raw=%d dropped=%d)",
			cacheFile,
			rawRows,
			droppedRows,
		)
		skippedDetails := cloneLogFields(aggregateDetails)
		skippedDetails["result"] = "skipped_no_complete_rows"
		logSilverPipelineTask("transform_cache_file", "success", transformStartedAt, skippedDetails, nil)
		return nil
	}

	silverSummaryPath := ""
	summaryStartedAt := time.Now()
	switch pipelineConfig.Pipeline.StorageBackend {
	case silverPipelineStorageLocal:
		silverSummaryPath = silverSummaryPathFromCacheFile(cacheFile)
		if err := writeSilverSummaryCSV(silverSummaryPath, aggregates, pipelineConfig.Pipeline.Transformation.MetricFields); err != nil {
			logSilverPipelineTask("write_silver_summary", "failed", summaryStartedAt, aggregateDetails, err)
			logSilverPipelineTask("transform_cache_file", "failed", transformStartedAt, aggregateDetails, err)
			return err
		}
	case silverPipelineStorageGCS:
		if gcsClient == nil {
			err := fmt.Errorf("gcs storage backend requires initialized client")
			logSilverPipelineTask("write_silver_summary", "failed", summaryStartedAt, aggregateDetails, err)
			logSilverPipelineTask("transform_cache_file", "failed", transformStartedAt, aggregateDetails, err)
			return err
		}
		silverSummaryPath, err = writeSilverSummaryGCSObject(ctx, gcsClient, cacheFile, aggregates, pipelineConfig.Pipeline.Transformation.MetricFields)
		if err != nil {
			logSilverPipelineTask("write_silver_summary", "failed", summaryStartedAt, aggregateDetails, err)
			logSilverPipelineTask("transform_cache_file", "failed", transformStartedAt, aggregateDetails, err)
			return err
		}
	default:
		err := fmt.Errorf("unsupported storage backend %q", pipelineConfig.Pipeline.StorageBackend)
		logSilverPipelineTask("write_silver_summary", "failed", summaryStartedAt, aggregateDetails, err)
		logSilverPipelineTask("transform_cache_file", "failed", transformStartedAt, aggregateDetails, err)
		return err
	}

	summaryDetails := cloneLogFields(aggregateDetails)
	summaryDetails["summary_csv"] = silverSummaryPath
	summaryDetails["summary_rows"] = len(aggregates)
	summarySizeBytes, summarySizeErr := storageAssetSize(ctx, gcsClient, silverSummaryPath)
	if summarySizeErr != nil {
		summaryDetails["summary_size_bytes"] = -1
		summaryDetails["summary_size_error"] = summarySizeErr.Error()
	} else {
		summaryDetails["summary_size_bytes"] = summarySizeBytes
	}
	logSilverPipelineTask("write_silver_summary", "success", summaryStartedAt, summaryDetails, nil)

	if err := enforceStorageSizeLimitWithTaskLog(ctx, pipelineConfig, gcsClient, "post-summary:"+bronzeTable); err != nil {
		logSilverPipelineTask("transform_cache_file", "failed", transformStartedAt, summaryDetails, err)
		return err
	}

	silverTable := silverTableName()
	tableStartedAt := time.Now()
	tableDetails := cloneLogFields(summaryDetails)
	tableDetails["silver_table"] = silverTable
	if err := createSilverTableIfNotExists(ctx, session, silverTable, pipelineConfig.Pipeline.Transformation.MetricFields, pipelineConfig.maxRetries()); err != nil {
		logSilverPipelineTask("ensure_silver_table", "failed", tableStartedAt, tableDetails, err)
		logSilverPipelineTask("transform_cache_file", "failed", transformStartedAt, tableDetails, err)
		return err
	}
	logSilverPipelineTask("ensure_silver_table", "success", tableStartedAt, tableDetails, nil)

	insertStartedAt := time.Now()
	insertedRows, err := insertSilverAggregates(ctx, session, silverTable, aggregates, pipelineConfig.Pipeline.Transformation.MetricFields, pipelineConfig.maxRetries())
	if err != nil {
		insertDetails := cloneLogFields(tableDetails)
		insertDetails["attempted_rows"] = len(aggregates)
		insertDetails["inserted_rows"] = insertedRows
		logSilverPipelineTask("insert_silver_aggregates", "failed", insertStartedAt, insertDetails, err)
		logSilverPipelineTask("transform_cache_file", "failed", transformStartedAt, insertDetails, err)
		return err
	}

	insertDetails := cloneLogFields(tableDetails)
	insertDetails["attempted_rows"] = len(aggregates)
	insertDetails["inserted_rows"] = insertedRows
	logSilverPipelineTask("insert_silver_aggregates", "success", insertStartedAt, insertDetails, nil)

	log.Printf(
		"Silver pipeline completed cache_file=%s bronze_table=%s silver_table=%s raw_rows=%d kept_rows=%d dropped_rows=%d silver_rows=%d summary_csv=%s metrics=%s",
		cacheFile,
		bronzeTable,
		silverTable,
		rawRows,
		keptRows,
		droppedRows,
		insertedRows,
		silverSummaryPath,
		strings.Join(pipelineConfig.Pipeline.Transformation.MetricFields, ","),
	)

	completedDetails := cloneLogFields(insertDetails)
	completedDetails["raw_rows"] = rawRows
	completedDetails["kept_rows"] = keptRows
	completedDetails["dropped_rows"] = droppedRows
	completedDetails["summary_csv"] = silverSummaryPath
	completedDetails["metric_fields"] = strings.Join(pipelineConfig.Pipeline.Transformation.MetricFields, ",")
	logSilverPipelineTask("transform_cache_file", "success", transformStartedAt, completedDetails, nil)

	return nil
}

func runSilverPipelineFromCache(ctx context.Context, session *gocql.Session, tenantConfig TenantConfig, pipelineConfig silverPipelineConfig, gcsClient *storage.Client, cacheFiles []string) error {
	startedAt := time.Now()
	details := map[string]any{"cache_files": len(cacheFiles)}

	for _, cacheFile := range cacheFiles {
		if err := processCachedBronzeFile(ctx, session, tenantConfig, pipelineConfig, gcsClient, cacheFile); err != nil {
			details["failed_cache_file"] = cacheFile
			logSilverPipelineTask("run_transform_from_cache", "failed", startedAt, details, err)
			return err
		}
	}

	logSilverPipelineTask("run_transform_from_cache", "success", startedAt, details, nil)

	return nil
}

func runSilverPipeline(ctx context.Context, session *gocql.Session, tenantConfig TenantConfig, pipelineConfig silverPipelineConfig, gcsClient *storage.Client) error {
	startedAt := time.Now()
	details := map[string]any{}

	extractedFiles, err := extractBronzeTablesToCache(ctx, session, tenantConfig, pipelineConfig, gcsClient)
	if err != nil {
		logSilverPipelineTask("run_full_pipeline", "failed", startedAt, details, err)
		return err
	}
	details["extracted_files"] = len(extractedFiles)

	err = runSilverPipelineFromCache(ctx, session, tenantConfig, pipelineConfig, gcsClient, extractedFiles)
	if err != nil {
		logSilverPipelineTask("run_full_pipeline", "failed", startedAt, details, err)
		return err
	}

	details["transformed_files"] = len(extractedFiles)
	logSilverPipelineTask("run_full_pipeline", "success", startedAt, details, nil)

	return nil
}

func main() {
	tenantID := activeSilverTenantID()
	if err := ensureSupportedSilverTenant(tenantID); err != nil {
		log.Fatalf("%v", err)
	}

	tenantConfig, err := loadTenantConfig(tenantID)
	if err != nil {
		log.Fatalf("failed to load tenant config for tenant=%s: %v", tenantID, err)
	}
	if err := ensureSupportedSilverTenant(tenantConfig.TenantID); err != nil {
		log.Fatalf("%v", err)
	}

	pipelineConfig, err := loadSilverPipelineConfig(tenantID)
	if err != nil {
		log.Fatalf("failed to load silver pipeline config for tenant=%s: %v", tenantID, err)
	}
	if err := validateStorageBackendConfig(pipelineConfig); err != nil {
		log.Fatalf("invalid silver pipeline storage configuration: %v", err)
	}
	if err := validatePipelineLoggingConfig(pipelineConfig); err != nil {
		log.Fatalf("invalid silver pipeline logging configuration: %v", err)
	}

	mode := silverPipelineMode()
	if mode != silverPipelineModeFull && mode != silverPipelineModeExtract && mode != silverPipelineModeTransform {
		log.Fatalf("invalid %s=%q", silverPipelineModeEnv, mode)
	}

	cluster := gocql.NewCluster(cassandraHosts...)
	cluster.Keyspace = cassandraKeyspace
	cluster.Consistency = cassandraConsistencyForSilverPipeline(tenantConfig.Tier)
	cluster.Timeout = 120 * time.Second
	cluster.NumConns = parsePositiveIntEnv("CASSANDRA_NUM_CONNS", defaultSilverPipelineNumConns)
	cluster.DisableInitialHostLookup = true

	session, err := cluster.CreateSession()
	if err != nil {
		log.Fatalf("failed to create Cassandra session for silver pipeline: %v", err)
	}
	defer session.Close()

	ctx, cancel := context.WithTimeout(context.Background(), pipelineConfig.maxRuntime())
	defer cancel()

	var gcsClient *storage.Client
	if pipelineConfig.Pipeline.StorageBackend == silverPipelineStorageGCS {
		gcsClient, err = newGCSClient(context.Background(), pipelineConfig)
		if err != nil {
			log.Fatalf("failed to initialize GCS storage backend: %v", err)
		}
		defer gcsClient.Close()
	}

	extractDayForLog := strings.TrimSpace(pipelineConfig.Pipeline.ExtractDay)
	if extractDayForLog == "" {
		extractDayForLog = "(not-set)"
	}
	runLogPath, taskLogPath := pipelineConfig.logOutputPaths()

	log.Printf(
		"Silver pipeline starting tenant=%s mode=%s keyspace=%s consistency=%s storage_backend=%s storage_target=%s extract_page_size=%d extract_day=%s metrics=%s max_runtime=%s max_retries=%d run_log=%s task_log=%s",
		tenantID,
		mode,
		cassandraKeyspace,
		cassandraConsistencyName(cluster.Consistency),
		pipelineConfig.Pipeline.StorageBackend,
		storageTargetForLog(pipelineConfig),
		pipelineConfig.Pipeline.ExtractPageSize,
		extractDayForLog,
		strings.Join(pipelineConfig.Pipeline.Transformation.MetricFields, ","),
		pipelineConfig.maxRuntime(),
		pipelineConfig.maxRetries(),
		runLogPath,
		taskLogPath,
	)

	pipelineLogger, err := newSilverPipelineFileLogger(pipelineConfig, tenantID, mode)
	if err != nil {
		log.Fatalf("failed to initialize silver pipeline file logger: %v", err)
	}
	activeSilverPipelineLogger = pipelineLogger
	defer func() {
		activeSilverPipelineLogger = nil
		pipelineLogger.Close()
	}()

	runStartedAt := time.Now()
	runDetails := map[string]any{
		"extract_day":       extractDayForLog,
		"extract_page_size": pipelineConfig.Pipeline.ExtractPageSize,
		"metric_fields":     strings.Join(pipelineConfig.Pipeline.Transformation.MetricFields, ","),
		"max_runtime_sec":   int(pipelineConfig.maxRuntime().Seconds()),
		"max_retries":       pipelineConfig.maxRetries(),
		"run_log_path":      runLogPath,
		"task_log_path":     taskLogPath,
	}
	pipelineLogger.logRunEvent("started", 0, runDetails, nil)

	var runErr error

	switch mode {
	case silverPipelineModeFull:
		if err := runSilverPipeline(ctx, session, tenantConfig, pipelineConfig, gcsClient); err != nil {
			runErr = fmt.Errorf("silver pipeline failed: %w", err)
		}
	case silverPipelineModeExtract:
		extractedFiles, err := extractBronzeTablesToCache(ctx, session, tenantConfig, pipelineConfig, gcsClient)
		if err != nil {
			runErr = fmt.Errorf("silver pipeline extract failed: %w", err)
			break
		}
		log.Printf("Silver pipeline extract completed: files=%d", len(extractedFiles))
	case silverPipelineModeTransform:
		resolveStartedAt := time.Now()
		cacheFiles, err := resolveTransformInputFiles(ctx, pipelineConfig, gcsClient)
		if err != nil {
			logSilverPipelineTask("resolve_transform_inputs", "failed", resolveStartedAt, nil, err)
			runErr = fmt.Errorf("silver pipeline transform-cache failed to resolve input files: %w", err)
			break
		}

		logSilverPipelineTask("resolve_transform_inputs", "success", resolveStartedAt, map[string]any{"matched_files": len(cacheFiles)}, nil)

		if len(cacheFiles) == 0 {
			log.Printf("Silver pipeline transform-cache found no matching input files in %s", storageTargetForLog(pipelineConfig))
		} else if err := runSilverPipelineFromCache(ctx, session, tenantConfig, pipelineConfig, gcsClient, cacheFiles); err != nil {
			runErr = fmt.Errorf("silver pipeline transform-cache failed: %w", err)
		}
	}

	if runErr == nil {
		if err := enforceStorageSizeLimitWithTaskLog(ctx, pipelineConfig, gcsClient, "pipeline-final"); err != nil {
			runErr = fmt.Errorf("silver pipeline cache validation failed: %w", err)
		}
	}

	if runErr != nil {
		pipelineLogger.logRunEvent("failed", time.Since(runStartedAt), runDetails, runErr)
		log.Fatalf("%v", runErr)
	}

	pipelineLogger.logRunEvent("success", time.Since(runStartedAt), runDetails, nil)

	log.Printf("Silver pipeline completed successfully for tenant=%s", tenantID)
}
