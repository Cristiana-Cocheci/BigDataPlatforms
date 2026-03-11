package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gocql/gocql"
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
	defaultSilverSummarySuffix        = "silver_hourly"
	defaultBatchmanagerInputGlob      = "*_bronze_extract.csv"
	defaultBatchmanagerStateFile      = ".batchmanager_state.json"
	silverPipelineModeEnv             = "SILVER_PIPELINE_MODE"
	silverPipelineInputFilesEnv       = "SILVER_PIPELINE_INPUT_FILES"
	silverPipelineDayEnv              = "SILVER_PIPELINE_DAY"
	silverPipelineModeFull            = "full"
	silverPipelineModeExtract         = "extract-cache"
	silverPipelineModeTransform       = "transform-cache"
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
	CacheDir        string                       `yaml:"cache_dir"`
	ExtractPageSize int                          `yaml:"extract_page_size"`
	ExtractDay      string                       `yaml:"extract_day"`
	Transformation  silverPipelineTransformation `yaml:"transformation"`
	BatchManager    silverPipelineBatchManager   `yaml:"batchmanager"`
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

func silverPipelineConfigPath(tenantID string) string {
	configDir := getEnv("TENANT_CONFIG_DIR", defaultTenantConfigDir)
	return filepath.Join(configDir, fmt.Sprintf("silverpipeline_%s.yaml", tenantID))
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

	if strings.TrimSpace(config.Pipeline.CacheDir) == "" {
		config.Pipeline.CacheDir = filepath.Join(defaultSilverPipelineCacheRoot, tenantID)
	}

	if config.Pipeline.ExtractPageSize < 1 {
		config.Pipeline.ExtractPageSize = defaultSilverPipelinePageSize
	}

	configuredExtractDay := strings.TrimSpace(config.Pipeline.ExtractDay)
	envExtractDay := strings.TrimSpace(os.Getenv(silverPipelineDayEnv))
	if envExtractDay != "" {
		config.Pipeline.ExtractDay = envExtractDay
	} else {
		config.Pipeline.ExtractDay = configuredExtractDay
	}

	config.Pipeline.Transformation.MetricFields = append([]string(nil), hardcodedTenant2MetricFields...)

	if strings.TrimSpace(config.Pipeline.BatchManager.InputGlob) == "" {
		config.Pipeline.BatchManager.InputGlob = defaultBatchmanagerInputGlob
	}

	if strings.TrimSpace(config.Pipeline.BatchManager.StateFile) == "" {
		config.Pipeline.BatchManager.StateFile = defaultBatchmanagerStateFile
	}
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
	case tenantTierGold:
		return gocql.Quorum
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

func listBronzeTables(ctx context.Context, session *gocql.Session, tablePrefix string) ([]string, error) {
	_ = ctx
	_ = session
	_ = tablePrefix
	return []string{hardcodedTenant2BronzeTable}, nil
}

func bronzeTableSuffix(tablePrefix string, tableName string) (string, error) {
	_ = tablePrefix
	if tableName != hardcodedTenant2BronzeTable {
		return "", fmt.Errorf("unexpected bronze table: got=%s want=%s", tableName, hardcodedTenant2BronzeTable)
	}
	return hardcodedTenant2TableSuffix, nil
}

func silverTableName(tablePrefix string, tableSuffix string) string {
	_ = tablePrefix
	_ = tableSuffix
	return fmt.Sprintf("%s.%s", cassandraKeyspace, hardcodedTenant2SilverTable)
}

func bronzeExtractCachePath(cacheDir string, tableName string, runID string) string {
	baseName := fmt.Sprintf("%s_%s_bronze_extract.csv", tableName, runID)
	return filepath.Join(cacheDir, baseName)
}

func silverSummaryCachePath(cacheDir string, tableName string, runID string) string {
	baseName := fmt.Sprintf("%s_%s_%s.csv", tableName, runID, defaultSilverSummarySuffix)
	return filepath.Join(cacheDir, baseName)
}

func discoverCachedBronzeFiles(cacheDir string, inputGlob string) ([]string, error) {
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

func resolveTransformInputFiles(cacheDir string, inputGlob string) ([]string, error) {
	rawInputs := strings.TrimSpace(os.Getenv(silverPipelineInputFilesEnv))
	if rawInputs == "" {
		return discoverCachedBronzeFiles(cacheDir, inputGlob)
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

func extractBronzeTableToCSV(ctx context.Context, session *gocql.Session, tenantConfig TenantConfig, tableName string, outputPath string, extractDay string, pageSize int) (int, error) {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return 0, fmt.Errorf("failed to create cache directory for %s: %w", outputPath, err)
	}

	file, err := os.Create(outputPath)
	if err != nil {
		return 0, fmt.Errorf("failed to create bronze cache file %s: %w", outputPath, err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	writer.Comma = ';'

	columnNames := make([]string, 0, len(tenantConfig.Schema.Columns))
	for _, column := range tenantConfig.Schema.Columns {
		columnNames = append(columnNames, normalizeSchemaToken(column.Name))
	}

	if err := writer.Write(columnNames); err != nil {
		return 0, fmt.Errorf("failed to write bronze cache header to %s: %w", outputPath, err)
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
		return rowCount, fmt.Errorf("failed to flush bronze cache file %s: %w", outputPath, err)
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

	reader := csv.NewReader(file)
	reader.Comma = ';'
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("failed to read cached bronze csv header %s: %w", csvPath, err)
	}

	columnIndexes := make(map[string]int, len(header))
	for idx, rawColumn := range header {
		columnIndexes[normalizeSchemaToken(rawColumn)] = idx
	}

	requiredColumns := append([]string{"day", "hour"}, metricFields...)
	for _, column := range requiredColumns {
		if _, exists := columnIndexes[column]; !exists {
			return nil, 0, 0, fmt.Errorf("cached bronze csv %s is missing required column %s", csvPath, column)
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
			return nil, droppedRows, keptRows, fmt.Errorf("failed to read cached bronze csv row from %s: %w", csvPath, err)
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

	writer := csv.NewWriter(file)
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
		return fmt.Errorf("failed to write silver summary header to %s: %w", outputPath, err)
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
			return fmt.Errorf("failed to write silver summary row to %s: %w", outputPath, err)
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("failed to flush silver summary csv %s: %w", outputPath, err)
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

func extractBronzeTablesToCache(ctx context.Context, session *gocql.Session, tenantConfig TenantConfig, pipelineConfig silverPipelineConfig) ([]string, error) {
	if err := os.MkdirAll(pipelineConfig.Pipeline.CacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create tenant cache directory %s: %w", pipelineConfig.Pipeline.CacheDir, err)
	}

	extractDay, err := normalizeExtractDay(pipelineConfig.Pipeline.ExtractDay)
	if err != nil {
		return nil, err
	}

	bronzeTables, err := listBronzeTables(ctx, session, tenantConfig.TablePrefix)
	if err != nil {
		return nil, err
	}

	runID := time.Now().UTC().Format("20060102_150405")
	extractedFiles := make([]string, 0, len(bronzeTables))
	for _, bronzeTable := range bronzeTables {
		bronzeCachePath := bronzeExtractCachePath(pipelineConfig.Pipeline.CacheDir, bronzeTable, runID)
		rawRows, err := extractBronzeTableToCSV(ctx, session, tenantConfig, bronzeTable, bronzeCachePath, extractDay, pipelineConfig.Pipeline.ExtractPageSize)
		if err != nil {
			return nil, err
		}

		if err := enforceCacheSizeLimit(pipelineConfig.Pipeline.CacheDir, pipelineConfig.PipelineConstraints.Storage.MaxSilverStorageGB); err != nil {
			return nil, err
		}

		log.Printf(
			"Silver pipeline extracted bronze_table=%s day=%s raw_rows=%d cache_csv=%s",
			bronzeTable,
			extractDay,
			rawRows,
			bronzeCachePath,
		)

		extractedFiles = append(extractedFiles, bronzeCachePath)
	}

	return extractedFiles, nil
}

func processCachedBronzeFile(ctx context.Context, session *gocql.Session, tenantConfig TenantConfig, pipelineConfig silverPipelineConfig, cacheFile string) error {
	bronzeTable, err := bronzeTableNameFromCacheFile(cacheFile)
	if err != nil {
		return err
	}

	tableSuffix, err := bronzeTableSuffix(tenantConfig.TablePrefix, bronzeTable)
	if err != nil {
		return err
	}

	aggregates, droppedRows, keptRows, err := aggregateHourlyCSV(
		cacheFile,
		pipelineConfig.Pipeline.Transformation.MetricFields,
		pipelineConfig.dropRowsWithMissingEntries(),
	)
	if err != nil {
		return err
	}

	rawRows := keptRows + droppedRows
	if len(aggregates) == 0 {
		log.Printf(
			"Silver pipeline skipped cache_file=%s because no complete rows remained after cleaning (raw=%d dropped=%d)",
			cacheFile,
			rawRows,
			droppedRows,
		)
		return nil
	}

	silverSummaryPath := silverSummaryPathFromCacheFile(cacheFile)
	if err := writeSilverSummaryCSV(silverSummaryPath, aggregates, pipelineConfig.Pipeline.Transformation.MetricFields); err != nil {
		return err
	}

	if err := enforceCacheSizeLimit(pipelineConfig.Pipeline.CacheDir, pipelineConfig.PipelineConstraints.Storage.MaxSilverStorageGB); err != nil {
		return err
	}

	silverTable := silverTableName(tenantConfig.TablePrefix, tableSuffix)
	if err := createSilverTableIfNotExists(ctx, session, silverTable, pipelineConfig.Pipeline.Transformation.MetricFields, pipelineConfig.maxRetries()); err != nil {
		return err
	}

	insertedRows, err := insertSilverAggregates(ctx, session, silverTable, aggregates, pipelineConfig.Pipeline.Transformation.MetricFields, pipelineConfig.maxRetries())
	if err != nil {
		return err
	}

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

	return nil
}

func runSilverPipelineFromCache(ctx context.Context, session *gocql.Session, tenantConfig TenantConfig, pipelineConfig silverPipelineConfig, cacheFiles []string) error {
	for _, cacheFile := range cacheFiles {
		if err := processCachedBronzeFile(ctx, session, tenantConfig, pipelineConfig, cacheFile); err != nil {
			return err
		}
	}

	return nil
}

func runSilverPipeline(ctx context.Context, session *gocql.Session, tenantConfig TenantConfig, pipelineConfig silverPipelineConfig) error {
	extractedFiles, err := extractBronzeTablesToCache(ctx, session, tenantConfig, pipelineConfig)
	if err != nil {
		return err
	}

	return runSilverPipelineFromCache(ctx, session, tenantConfig, pipelineConfig, extractedFiles)
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

	extractDayForLog := strings.TrimSpace(pipelineConfig.Pipeline.ExtractDay)
	if extractDayForLog == "" {
		extractDayForLog = "(not-set)"
	}

	log.Printf(
		"Silver pipeline starting tenant=%s mode=%s keyspace=%s consistency=%s cache_dir=%s extract_page_size=%d extract_day=%s metrics=%s max_runtime=%s max_retries=%d",
		tenantID,
		mode,
		cassandraKeyspace,
		cassandraConsistencyName(cluster.Consistency),
		pipelineConfig.Pipeline.CacheDir,
		pipelineConfig.Pipeline.ExtractPageSize,
		extractDayForLog,
		strings.Join(pipelineConfig.Pipeline.Transformation.MetricFields, ","),
		pipelineConfig.maxRuntime(),
		pipelineConfig.maxRetries(),
	)

	switch mode {
	case silverPipelineModeFull:
		if err := runSilverPipeline(ctx, session, tenantConfig, pipelineConfig); err != nil {
			log.Fatalf("silver pipeline failed: %v", err)
		}
	case silverPipelineModeExtract:
		extractedFiles, err := extractBronzeTablesToCache(ctx, session, tenantConfig, pipelineConfig)
		if err != nil {
			log.Fatalf("silver pipeline extract failed: %v", err)
		}
		log.Printf("Silver pipeline extract completed: files=%d", len(extractedFiles))
	case silverPipelineModeTransform:
		cacheFiles, err := resolveTransformInputFiles(pipelineConfig.Pipeline.CacheDir, pipelineConfig.Pipeline.BatchManager.InputGlob)
		if err != nil {
			log.Fatalf("silver pipeline transform-cache failed to resolve input files: %v", err)
		}
		if len(cacheFiles) == 0 {
			log.Printf("Silver pipeline transform-cache found no matching input files in %s", pipelineConfig.Pipeline.CacheDir)
		} else if err := runSilverPipelineFromCache(ctx, session, tenantConfig, pipelineConfig, cacheFiles); err != nil {
			log.Fatalf("silver pipeline transform-cache failed: %v", err)
		}
	}

	if err := enforceCacheSizeLimit(pipelineConfig.Pipeline.CacheDir, pipelineConfig.PipelineConstraints.Storage.MaxSilverStorageGB); err != nil {
		log.Fatalf("silver pipeline cache validation failed: %v", err)
	}

	log.Printf("Silver pipeline completed successfully for tenant=%s", tenantID)
}
