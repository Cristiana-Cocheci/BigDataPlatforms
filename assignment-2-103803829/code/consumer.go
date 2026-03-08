package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"github.com/segmentio/kafka-go"
)

type tenantSchemaProfile struct {
	Name             string
	TablePrefix      string
	TableSuffixField string
	Columns          []TenantSchemaColumnConfig
	PartitionKeys    []string
	ClusteringKeys   []string
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

func activeTenantID() string {
	tenantID := strings.ToLower(strings.TrimSpace(os.Getenv("TENANT_ID")))
	if tenantID == "" {
		return "tenant1"
	}
	return tenantID
}

func activeWorkerID() string {
	if workerID := strings.TrimSpace(os.Getenv("WORKER_ID")); workerID != "" {
		return workerID
	}

	if hostname := strings.TrimSpace(os.Getenv("HOSTNAME")); hostname != "" {
		return hostname
	}

	return "worker-unknown"
}

func schemaForTenant(config TenantConfig) (tenantSchemaProfile, error) {
	schemaName := strings.TrimSpace(config.SchemaProfile)
	if schemaName == "" {
		schemaName = "custom"
	}

	tablePrefix := strings.TrimSpace(config.TablePrefix)
	if tablePrefix == "" {
		return tenantSchemaProfile{}, fmt.Errorf("table_prefix is required")
	}

	if len(config.Schema.Columns) == 0 {
		return tenantSchemaProfile{}, fmt.Errorf("schema.columns must contain at least one column")
	}

	if len(config.Schema.PrimaryKey.Partition) == 0 {
		return tenantSchemaProfile{}, fmt.Errorf("schema.primary_key.partition must contain at least one column")
	}

	columns := make([]TenantSchemaColumnConfig, 0, len(config.Schema.Columns))
	columnNames := make(map[string]struct{}, len(config.Schema.Columns))
	fieldNames := make(map[string]struct{}, len(config.Schema.Columns))

	for _, rawColumn := range config.Schema.Columns {
		columnName := normalizeSchemaToken(rawColumn.Name)
		columnType := strings.TrimSpace(rawColumn.Type)
		measurementField := normalizeSchemaToken(rawColumn.Field)

		if columnName == "" {
			return tenantSchemaProfile{}, fmt.Errorf("schema column name cannot be empty")
		}

		if columnType == "" {
			return tenantSchemaProfile{}, fmt.Errorf("schema column type cannot be empty for column=%s", columnName)
		}

		if measurementField == "" {
			return tenantSchemaProfile{}, fmt.Errorf("schema field mapping cannot be empty for column=%s", columnName)
		}

		if _, exists := columnNames[columnName]; exists {
			return tenantSchemaProfile{}, fmt.Errorf("duplicate schema column=%s", columnName)
		}

		columnNames[columnName] = struct{}{}
		fieldNames[measurementField] = struct{}{}
		columns = append(columns, TenantSchemaColumnConfig{
			Name:  columnName,
			Type:  columnType,
			Field: measurementField,
		})
	}

	partitionKeys := make([]string, 0, len(config.Schema.PrimaryKey.Partition))
	for _, rawKey := range config.Schema.PrimaryKey.Partition {
		key := normalizeSchemaToken(rawKey)
		if key == "" {
			return tenantSchemaProfile{}, fmt.Errorf("partition key cannot be empty")
		}

		if _, ok := columnNames[key]; !ok {
			return tenantSchemaProfile{}, fmt.Errorf("partition key %s is not defined in schema.columns", key)
		}

		partitionKeys = append(partitionKeys, key)
	}

	clusteringKeys := make([]string, 0, len(config.Schema.PrimaryKey.Clustering))
	for _, rawKey := range config.Schema.PrimaryKey.Clustering {
		key := normalizeSchemaToken(rawKey)
		if key == "" {
			return tenantSchemaProfile{}, fmt.Errorf("clustering key cannot be empty")
		}

		if _, ok := columnNames[key]; !ok {
			return tenantSchemaProfile{}, fmt.Errorf("clustering key %s is not defined in schema.columns", key)
		}

		clusteringKeys = append(clusteringKeys, key)
	}

	tableSuffixField := normalizeSchemaToken(config.Schema.TableSuffixField)
	if tableSuffixField == "" {
		return tenantSchemaProfile{}, fmt.Errorf("schema.table_suffix_field cannot be empty")
	}

	if _, exists := fieldNames[tableSuffixField]; !exists {
		return tenantSchemaProfile{}, fmt.Errorf("schema.table_suffix_field=%s must match one schema.columns[].field", tableSuffixField)
	}

	return tenantSchemaProfile{
		Name:             schemaName,
		TablePrefix:      tablePrefix,
		TableSuffixField: tableSuffixField,
		Columns:          columns,
		PartitionKeys:    partitionKeys,
		ClusteringKeys:   clusteringKeys,
	}, nil
}

func normalizeSchemaToken(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	normalized = strings.ReplaceAll(normalized, " ", "_")
	return normalized
}

func primaryKeyClause(partitionKeys []string, clusteringKeys []string) string {
	partition := ""
	if len(partitionKeys) == 1 {
		partition = partitionKeys[0]
	} else {
		partition = fmt.Sprintf("(%s)", strings.Join(partitionKeys, ", "))
	}

	if len(clusteringKeys) == 0 {
		return partition
	}

	keys := make([]string, 0, 1+len(clusteringKeys))
	keys = append(keys, partition)
	keys = append(keys, clusteringKeys...)
	return strings.Join(keys, ", ")
}

func createTableCQL(schema tenantSchemaProfile, tableName string) string {
	columnDefs := make([]string, 0, len(schema.Columns)+1)
	for _, column := range schema.Columns {
		columnDefs = append(columnDefs, fmt.Sprintf("%s %s", column.Name, column.Type))
	}

	columnDefs = append(columnDefs, fmt.Sprintf("PRIMARY KEY (%s)", primaryKeyClause(schema.PartitionKeys, schema.ClusteringKeys)))
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)", tableName, strings.Join(columnDefs, ", "))
}

func insertCQL(schema tenantSchemaProfile, tableName string) string {
	columnNames := make([]string, 0, len(schema.Columns))
	placeholders := make([]string, 0, len(schema.Columns))

	for _, column := range schema.Columns {
		columnNames = append(columnNames, column.Name)
		placeholders = append(placeholders, "?")
	}

	return fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		tableName,
		strings.Join(columnNames, ", "),
		strings.Join(placeholders, ", "),
	)
}

func parseKafkaRecord(payload []byte) (map[string]any, error) {
	var record map[string]any
	if err := json.Unmarshal(payload, &record); err != nil {
		return nil, err
	}

	normalizedRecord := make(map[string]any, len(record))
	for key, value := range record {
		normalizedRecord[normalizeSchemaToken(key)] = value
	}

	return normalizedRecord, nil
}

func bindRecordValues(schema tenantSchemaProfile, record map[string]any) ([]any, error) {
	values := make([]any, 0, len(schema.Columns))

	for _, column := range schema.Columns {
		rawValue, exists := record[column.Field]
		if !exists {
			if isPrimaryKeyColumn(schema, column.Name) {
				return nil, fmt.Errorf("missing required primary key field=%s", column.Field)
			}

			values = append(values, nil)
			continue
		}

		if rawValue == nil {
			if isPrimaryKeyColumn(schema, column.Name) {
				return nil, fmt.Errorf("primary key field=%s cannot be null", column.Field)
			}

			values = append(values, nil)
			continue
		}

		value, err := coerceValueForCQL(rawValue, column.Type)
		if err != nil {
			return nil, fmt.Errorf("column=%s field=%s type=%s: %w", column.Name, column.Field, column.Type, err)
		}

		values = append(values, value)
	}

	return values, nil
}

func isPrimaryKeyColumn(schema tenantSchemaProfile, columnName string) bool {
	normalizedColumnName := normalizeSchemaToken(columnName)

	for _, key := range schema.PartitionKeys {
		if key == normalizedColumnName {
			return true
		}
	}

	for _, key := range schema.ClusteringKeys {
		if key == normalizedColumnName {
			return true
		}
	}

	return false
}

func coerceValueForCQL(rawValue any, cqlType string) (any, error) {
	normalizedType := strings.ToLower(strings.TrimSpace(cqlType))

	switch normalizedType {
	case "int":
		return asInt(rawValue)
	case "bigint", "counter":
		return asInt64(rawValue)
	case "float":
		return asFloat32(rawValue)
	case "double", "decimal":
		return asFloat64(rawValue)
	case "boolean", "bool":
		return asBool(rawValue)
	case "timestamp":
		return asTimestamp(rawValue)
	case "text", "varchar", "ascii":
		return fmt.Sprintf("%v", rawValue), nil
	default:
		return rawValue, nil
	}
}

func asInt(rawValue any) (int, error) {
	switch value := rawValue.(type) {
	case int:
		return value, nil
	case int32:
		return int(value), nil
	case int64:
		return int(value), nil
	case float64:
		if math.Trunc(value) != value {
			return 0, fmt.Errorf("value=%v is not an integer", value)
		}
		return int(value), nil
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, err
		}
		return int(parsed), nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, err
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("cannot convert %T to int", rawValue)
	}
}

func asInt64(rawValue any) (int64, error) {
	switch value := rawValue.(type) {
	case int:
		return int64(value), nil
	case int32:
		return int64(value), nil
	case int64:
		return value, nil
	case float64:
		if math.Trunc(value) != value {
			return 0, fmt.Errorf("value=%v is not an integer", value)
		}
		return int64(value), nil
	case json.Number:
		return value.Int64()
	case string:
		return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to int64", rawValue)
	}
}

func asFloat32(rawValue any) (float32, error) {
	switch value := rawValue.(type) {
	case float32:
		return value, nil
	case float64:
		return float32(value), nil
	case int:
		return float32(value), nil
	case int32:
		return float32(value), nil
	case int64:
		return float32(value), nil
	case json.Number:
		parsed, err := value.Float64()
		if err != nil {
			return 0, err
		}
		return float32(parsed), nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 32)
		if err != nil {
			return 0, err
		}
		return float32(parsed), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to float32", rawValue)
	}
}

func asFloat64(rawValue any) (float64, error) {
	switch value := rawValue.(type) {
	case float32:
		return float64(value), nil
	case float64:
		return value, nil
	case int:
		return float64(value), nil
	case int32:
		return float64(value), nil
	case int64:
		return float64(value), nil
	case json.Number:
		return value.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(value), 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", rawValue)
	}
}

func asBool(rawValue any) (bool, error) {
	switch value := rawValue.(type) {
	case bool:
		return value, nil
	case string:
		return strconv.ParseBool(strings.TrimSpace(value))
	case int:
		return value != 0, nil
	case int32:
		return value != 0, nil
	case int64:
		return value != 0, nil
	case float64:
		return value != 0, nil
	default:
		return false, fmt.Errorf("cannot convert %T to bool", rawValue)
	}
}

func asTimestamp(rawValue any) (time.Time, error) {
	switch value := rawValue.(type) {
	case time.Time:
		return value, nil
	case string:
		trimmedValue := strings.TrimSpace(value)
		if trimmedValue == "" {
			return time.Time{}, fmt.Errorf("empty timestamp string")
		}

		if parsed, err := time.Parse(time.RFC3339, trimmedValue); err == nil {
			return parsed, nil
		}

		if parsed, err := time.Parse("2006-01-02T15:04:05", trimmedValue); err == nil {
			return parsed.UTC(), nil
		}

		return time.Time{}, fmt.Errorf("cannot parse timestamp string=%s", trimmedValue)
	case int64:
		return time.UnixMilli(value).UTC(), nil
	case int:
		return time.UnixMilli(int64(value)).UTC(), nil
	case float64:
		if math.Trunc(value) != value {
			return time.Time{}, fmt.Errorf("timestamp epoch value must be integer, got=%v", value)
		}
		return time.UnixMilli(int64(value)).UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("cannot convert %T to timestamp", rawValue)
	}
}

func tableSuffixFromRecord(schema tenantSchemaProfile, record map[string]any) string {
	rawValue, exists := record[schema.TableSuffixField]
	if !exists || rawValue == nil {
		return "default"
	}

	tableSuffix := strings.TrimSpace(fmt.Sprintf("%v", rawValue))
	if tableSuffix == "" {
		return "default"
	}

	return tableSuffix
}

func float32ToText(value *float32) any {
	if value == nil {
		return nil
	}
	return strconv.FormatFloat(float64(*value), 'f', -1, 32)
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

func validateManagedWorkerContract() error {
	required := []string{
		"TENANT_ID",
		"KAFKA_BROKERS",
		"KAFKA_TOPIC",
		"KAFKA_CONSUMER_GROUP",
		"CASSANDRA_KEYSPACE",
		"CASSANDRA_HOSTS",
	}

	missing := make([]string, 0)
	for _, key := range required {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			missing = append(missing, key)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("managed worker contract violation: missing env %s", strings.Join(missing, ", "))
	}

	return nil
}

func getTableName(schema tenantSchemaProfile, tableSuffix string) string {
	safeTableSuffix := sanitizeIdentifierPart(tableSuffix, "default")
	return fmt.Sprintf("%s.%s_%s_bronze", cassandraKeyspace, schema.TablePrefix, safeTableSuffix)
}

func createTableIfNotExists(session *gocql.Session, schema tenantSchemaProfile, tableSuffix string) (string, error) {
	tableName := getTableName(schema, tableSuffix)
	createQuery := createTableCQL(schema, tableName)

	if err := session.Query(createQuery).Exec(); err != nil {
		return "", fmt.Errorf("failed to create table %s: %w", tableName, err)
	}

	log.Printf("Created table using schema=%s: %s", schema.Name, tableName)
	return tableName, nil
}

func insertBatch(session *gocql.Session, schema tenantSchemaProfile, tableName string, records []map[string]any) error {
	if len(records) == 0 {
		return nil
	}

	// Use UnloggedBatch for better performance
	batch := session.NewBatch(gocql.UnloggedBatch)

	insertQuery := insertCQL(schema, tableName)

	for _, record := range records {
		values, err := bindRecordValues(schema, record)
		if err != nil {
			return fmt.Errorf("failed to bind values for table %s: %w", tableName, err)
		}

		batch.Query(insertQuery, values...)
	}

	if err := session.ExecuteBatch(batch); err != nil {
		return fmt.Errorf("failed to insert batch into %s: %w", tableName, err)
	}

	return nil
}

func consumeMessages(session *gocql.Session) error {
	startTime := time.Now()
	tenantID := activeTenantID()
	workerID := activeWorkerID()
	tenantConfig, err := loadTenantConfig(tenantID)
	if err != nil {
		return fmt.Errorf("failed to load tenant config for tenant=%s: %w", tenantID, err)
	}

	schema, err := schemaForTenant(tenantConfig)
	if err != nil {
		return fmt.Errorf("invalid tenant schema for tenant=%s: %w", tenantID, err)
	}

	log.Printf(
		"Tenant schema selected: tenant=%s profile=%s format=%s table_prefix=%s columns=%d",
		tenantConfig.TenantID,
		schema.Name,
		tenantConfig.CSVFormat,
		schema.TablePrefix,
		len(schema.Columns),
	)

	logInterval := 10 * time.Second
	if v := os.Getenv("THROUGHPUT_LOG_SECONDS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			logInterval = time.Duration(secs) * time.Second
		}
	}

	reportURL := strings.TrimSpace(os.Getenv("MONITOR_REPORT_URL"))
	reportInterval := 15 * time.Second
	if v := strings.TrimSpace(os.Getenv("MONITOR_REPORT_INTERVAL_SECONDS")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			reportInterval = time.Duration(secs) * time.Second
		}
	}

	reportClient := &http.Client{Timeout: 5 * time.Second}
	lastReportTime := time.Now()
	windowInsertStart := 0
	windowBatchLatencyMS := 0.0
	windowBatchCount := 0

	if reportURL == "" {
		log.Println("Monitor reporting disabled: MONITOR_REPORT_URL is empty")
	} else {
		log.Printf("Monitor reporting enabled: url=%s interval=%s worker=%s", reportURL, reportInterval, workerID)
	}

	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        []string{kafkaBrokers},
		Topic:          kafkaTopic,
		GroupID:        kafkaConsumerGroup,
		CommitInterval: time.Second,
		MaxBytes:       10e6,
	})
	defer r.Close()

	log.Println("Kafka consumer connected, consuming from topic:", kafkaTopic)

	// Read first message to determine table suffix and create table
	firstMsg, err := r.ReadMessage(context.Background())
	if err != nil {
		return fmt.Errorf("failed to read first message: %w", err)
	}

	firstRecord, err := parseKafkaRecord(firstMsg.Value)
	if err != nil {
		return fmt.Errorf("failed to unmarshal first message: %w", err)
	}
	tableSuffix := tableSuffixFromRecord(schema, firstRecord)
	tableName := getTableName(schema, tableSuffix)

	log.Printf("Detected table_suffix_field=%s value=%s, creating table with profile=%s: %s", schema.TableSuffixField, tableSuffix, schema.Name, tableName)

	// Create table once at the beginning
	createdTableName, err := createTableIfNotExists(session, schema, tableSuffix)
	if err != nil {
		return err
	}
	tableName = createdTableName

	// Start consuming with the first message already read
	batch := make([]map[string]any, 0, batchSize)
	messageCount := 1
	insertCount := 0
	lastLogTime := time.Now()
	lastLogInsert := 0

	// Add first record to batch
	batch = append(batch, firstRecord)

	for {
		msg, err := r.ReadMessage(context.Background())
		if err != nil {
			log.Printf("Error reading message: %v", err)
			break
		}

		messageCount++

		// Parse JSON as dynamic tenant record
		record, err := parseKafkaRecord(msg.Value)
		if err != nil {
			log.Printf("Warning: Failed to unmarshal message %d: %v", messageCount, err)
			continue
		}

		batch = append(batch, record)

		// Insert batch when size reached
		if len(batch) >= batchSize {
			batchStart := time.Now()
			if err := insertBatch(session, schema, tableName, batch); err != nil {
				return fmt.Errorf("failed to insert batch: %w", err)
			}
			windowBatchLatencyMS += time.Since(batchStart).Seconds() * 1000
			windowBatchCount++
			insertCount += len(batch)
			log.Printf("Inserted %d records (total: %d, consumed messages: %d)", len(batch), insertCount, messageCount)
			batch = make([]map[string]any, 0, batchSize)
		}

		if time.Since(lastLogTime) >= logInterval {
			elapsed := time.Since(lastLogTime).Seconds()
			delta := insertCount - lastLogInsert
			rate := float64(delta) / elapsed
			log.Printf("Throughput: %.2f records/s over %.1fs (total inserted: %d)", rate, elapsed, insertCount)
			lastLogTime = time.Now()
			lastLogInsert = insertCount
		}

		if reportURL != "" && time.Since(lastReportTime) >= reportInterval {
			reportWindowSeconds := time.Since(lastReportTime).Seconds()
			recordsInWindow := insertCount - windowInsertStart
			if reportWindowSeconds > 0 && recordsInWindow >= 0 {
				avgBatchMS := 0.0
				if windowBatchCount > 0 {
					avgBatchMS = windowBatchLatencyMS / float64(windowBatchCount)
				}

				report := workerPerformanceReport{
					TenantID:                tenantID,
					WorkerID:                workerID,
					KafkaTopic:              kafkaTopic,
					ReportedAt:              time.Now().UTC().Format(time.RFC3339),
					WindowSeconds:           reportWindowSeconds,
					RecordsInWindow:         recordsInWindow,
					BatchesInWindow:         windowBatchCount,
					AvgBatchIngestMS:        avgBatchMS,
					ThroughputRecordsPerSec: float64(recordsInWindow) / reportWindowSeconds,
					TotalInserted:           insertCount,
					TotalConsumed:           messageCount,
				}

				if err := sendPerformanceReport(reportClient, reportURL, report); err != nil {
					log.Printf("Monitor report failed: tenant=%s worker=%s err=%v", tenantID, workerID, err)
				}
			}

			lastReportTime = time.Now()
			windowInsertStart = insertCount
			windowBatchLatencyMS = 0
			windowBatchCount = 0
		}
	}

	// Insert remaining records
	if len(batch) > 0 {
		batchStart := time.Now()
		if err := insertBatch(session, schema, tableName, batch); err != nil {
			return fmt.Errorf("failed to insert final batch: %w", err)
		}
		windowBatchLatencyMS += time.Since(batchStart).Seconds() * 1000
		windowBatchCount++
		insertCount += len(batch)
		log.Printf("Inserted %d records (total: %d, consumed messages: %d, time_since_start %.2fs)", len(batch), insertCount, messageCount, time.Since(startTime).Seconds())
	}

	if reportURL != "" {
		reportWindowSeconds := time.Since(lastReportTime).Seconds()
		recordsInWindow := insertCount - windowInsertStart
		if reportWindowSeconds > 0 && recordsInWindow >= 0 {
			avgBatchMS := 0.0
			if windowBatchCount > 0 {
				avgBatchMS = windowBatchLatencyMS / float64(windowBatchCount)
			}

			report := workerPerformanceReport{
				TenantID:                tenantID,
				WorkerID:                workerID,
				KafkaTopic:              kafkaTopic,
				ReportedAt:              time.Now().UTC().Format(time.RFC3339),
				WindowSeconds:           reportWindowSeconds,
				RecordsInWindow:         recordsInWindow,
				BatchesInWindow:         windowBatchCount,
				AvgBatchIngestMS:        avgBatchMS,
				ThroughputRecordsPerSec: float64(recordsInWindow) / reportWindowSeconds,
				TotalInserted:           insertCount,
				TotalConsumed:           messageCount,
			}

			if err := sendPerformanceReport(reportClient, reportURL, report); err != nil {
				log.Printf("Final monitor report failed: tenant=%s worker=%s err=%v", tenantID, workerID, err)
			}
		}
	}

	duration := time.Since(startTime)
	throughput := float64(insertCount) / duration.Seconds()
	log.Printf("Consumption complete! Total records inserted: %d, Total messages consumed: %d", insertCount, messageCount)
	log.Printf("Performance: Duration=%.2fs, Throughput=%.2f records/s", duration.Seconds(), throughput)
	return nil
}

func sendPerformanceReport(client *http.Client, reportURL string, report workerPerformanceReport) error {
	payload, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("failed to marshal report: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, reportURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create report request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send report: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("monitor returned status %d", resp.StatusCode)
	}

	return nil
}

func main() {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("WORKER_MODE")), "managed") {
		if err := validateManagedWorkerContract(); err != nil {
			log.Fatalf("%v", err)
		}

		log.Printf(
			"managed worker init: tenant=%s topic=%s group=%s keyspace=%s brokers=%s",
			os.Getenv("TENANT_ID"),
			kafkaTopic,
			kafkaConsumerGroup,
			cassandraKeyspace,
			kafkaBrokers,
		)
	}

	// Create cluster
	cluster := gocql.NewCluster(cassandraHosts...)
	// cluster := gocql.NewCluster("cassandra1", "cassandra2", "cassandra3", "cassandra4", "cassandra5") // Add more nodes for better performance
	cluster.Keyspace = cassandraKeyspace
	cluster.Consistency = gocql.Quorum
	// cluster.Consistency = gocql.One
	//cluster.Consistency = gocql.All
	cluster.Timeout = 120 * time.Second
	// Increase connection pool for better throughput
	cluster.NumConns = 4
	// Disable initial host lookup to speed up connection
	cluster.DisableInitialHostLookup = true

	// Create session
	session, err := cluster.CreateSession()
	if err != nil {
		log.Fatalf("Failed to create session: %v", err)
	}
	defer session.Close()

	log.Println("Connected to Cassandra cluster")

	if err := consumeMessages(session); err != nil {
		log.Fatalf("Consumer error: %v", err)
	}
}
