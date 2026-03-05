package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"github.com/segmentio/kafka-go"
)

type tenantSchemaProfile struct {
	Name           string
	TablePrefix    string
	CreateTableCQL func(tableName string) string
	InsertCQL      func(tableName string) string
	BindValues     func(measurement *Measurement) []any
}

func activeTenantID() string {
	tenantID := strings.ToLower(strings.TrimSpace(os.Getenv("TENANT_ID")))
	if tenantID == "" {
		return "tenant1"
	}
	return tenantID
}

func schemaForTenant(config TenantConfig) tenantSchemaProfile {
	switch strings.ToLower(strings.TrimSpace(config.CSVFormat)) {
	case csvFormatDHT22Compact:
		return dht22CompactSchema(config)
	case csvFormatBME280Full:
		fallthrough
	default:
		return bme280Schema(config)
	}
}

func bme280Schema(config TenantConfig) tenantSchemaProfile {
	schemaName := strings.TrimSpace(config.SchemaProfile)
	if schemaName == "" {
		schemaName = "bme280"
	}

	tablePrefix := strings.TrimSpace(config.TablePrefix)
	if tablePrefix == "" {
		tablePrefix = "sensor_measurements"
	}

	return tenantSchemaProfile{
		Name:        schemaName,
		TablePrefix: tablePrefix,
		CreateTableCQL: func(tableName string) string {
			return fmt.Sprintf(`
				CREATE TABLE IF NOT EXISTS %s (
					sensor_id int,
					sensor_type text,
					location float,
					lat float,
					lon float,
					day text,
					hour int,
					timestamp text,
					pressure float,
					altitude float,
					pressure_sealevel float,
					temperature float,
					humidity float,
					PRIMARY KEY ((day, hour), sensor_id, timestamp)
				)
			`, tableName)
		},
		InsertCQL: func(tableName string) string {
			return fmt.Sprintf(
				"INSERT INTO %s (sensor_id, sensor_type, location, lat, lon, day, hour, timestamp, pressure, altitude, pressure_sealevel, temperature, humidity) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				tableName,
			)
		},
		BindValues: func(measurement *Measurement) []any {
			return []any{
				measurement.sensor_id,
				measurement.sensor_type,
				measurement.location,
				measurement.lat,
				measurement.lon,
				measurement.day,
				measurement.hour,
				measurement.timestamp,
				measurement.pressure,
				measurement.altitude,
				measurement.pressure_sealevel,
				measurement.temperature,
				measurement.humidity,
			}
		},
	}
}

func dht22CompactSchema(config TenantConfig) tenantSchemaProfile {
	schemaName := strings.TrimSpace(config.SchemaProfile)
	if schemaName == "" {
		schemaName = "dht22"
	}

	tablePrefix := strings.TrimSpace(config.TablePrefix)
	if tablePrefix == "" {
		tablePrefix = "sensor_observations"
	}

	return tenantSchemaProfile{
		Name:        schemaName,
		TablePrefix: tablePrefix,
		CreateTableCQL: func(tableName string) string {
			return fmt.Sprintf(`
				CREATE TABLE IF NOT EXISTS %s (
					sensor_id int,
					sensor_type text,
					location float,
					lat float,
					lon float,
					day text,
					hour int,
					timestamp text,
					temperature float,
					humidity float,
					PRIMARY KEY ((day, hour), sensor_id, timestamp)
				)
			`, tableName)
		},
		InsertCQL: func(tableName string) string {
			return fmt.Sprintf(
				"INSERT INTO %s (sensor_id, sensor_type, location, lat, lon, day, hour, timestamp, temperature, humidity) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				tableName,
			)
		},
		BindValues: func(measurement *Measurement) []any {
			return []any{
				measurement.sensor_id,
				measurement.sensor_type,
				measurement.location,
				measurement.lat,
				measurement.lon,
				measurement.day,
				measurement.hour,
				measurement.timestamp,
				measurement.temperature,
				measurement.humidity,
			}
		},
	}
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

func getTableName(schema tenantSchemaProfile, sensorType string) string {
	safeSensorType := sanitizeIdentifierPart(sensorType, "unknown_sensor")
	return fmt.Sprintf("%s.%s_%s_bronze", cassandraKeyspace, schema.TablePrefix, safeSensorType)
}

func createTableIfNotExists(session *gocql.Session, schema tenantSchemaProfile, sensorType string) (string, error) {
	tableName := getTableName(schema, sensorType)
	createQuery := schema.CreateTableCQL(tableName)

	if err := session.Query(createQuery).Exec(); err != nil {
		return "", fmt.Errorf("failed to create table %s: %w", tableName, err)
	}

	log.Printf("Created table using schema=%s: %s", schema.Name, tableName)
	return tableName, nil
}

func insertBatch(session *gocql.Session, schema tenantSchemaProfile, tableName string, measurements []*Measurement) error {
	if len(measurements) == 0 {
		return nil
	}

	// Use UnloggedBatch for better performance
	batch := session.NewBatch(gocql.UnloggedBatch)

	insertQuery := schema.InsertCQL(tableName)

	for _, m := range measurements {
		batch.Query(insertQuery, schema.BindValues(m)...)
	}

	if err := session.ExecuteBatch(batch); err != nil {
		return fmt.Errorf("failed to insert batch into %s: %w", tableName, err)
	}

	return nil
}

func consumeMessages(session *gocql.Session) error {
	startTime := time.Now()
	tenantID := activeTenantID()
	tenantConfig, err := loadTenantConfig(tenantID)
	if err != nil {
		return fmt.Errorf("failed to load tenant config for tenant=%s: %w", tenantID, err)
	}

	schema := schemaForTenant(tenantConfig)
	log.Printf(
		"Tenant schema selected: tenant=%s profile=%s format=%s table_prefix=%s",
		tenantConfig.TenantID,
		schema.Name,
		tenantConfig.CSVFormat,
		schema.TablePrefix,
	)

	logInterval := 10 * time.Second
	if v := os.Getenv("THROUGHPUT_LOG_SECONDS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			logInterval = time.Duration(secs) * time.Second
		}
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

	// Read first message to determine sensor_type and day for table creation
	firstMsg, err := r.ReadMessage(context.Background())
	if err != nil {
		return fmt.Errorf("failed to read first message: %w", err)
	}

	var mj MeasurementJSON
	if err := json.Unmarshal(firstMsg.Value, &mj); err != nil {
		return fmt.Errorf("failed to unmarshal first message: %w", err)
	}

	firstMeasurement, err := jsonToMeasurement(&mj)
	if err != nil {
		return fmt.Errorf("failed to convert first message: %w", err)
	}

	sensorType := firstMeasurement.sensor_type
	day := firstMeasurement.day
	tableName := getTableName(schema, sensorType)

	log.Printf("Detected sensor_type=%s, day=%s, creating table with profile=%s: %s", sensorType, day, schema.Name, tableName)

	// Create table once at the beginning
	createdTableName, err := createTableIfNotExists(session, schema, sensorType)
	if err != nil {
		return err
	}
	tableName = createdTableName

	// Start consuming with the first message already read
	batch := make([]*Measurement, 0, batchSize)
	messageCount := 1
	insertCount := 0
	lastLogTime := time.Now()
	lastLogInsert := 0

	// Add first measurement to batch
	batch = append(batch, firstMeasurement)

	for {
		msg, err := r.ReadMessage(context.Background())
		if err != nil {
			log.Printf("Error reading message: %v", err)
			break
		}

		messageCount++

		// Parse JSON
		var mj MeasurementJSON
		if err := json.Unmarshal(msg.Value, &mj); err != nil {
			log.Printf("Warning: Failed to unmarshal message %d: %v", messageCount, err)
			continue
		}

		// Convert to Measurement
		m, err := jsonToMeasurement(&mj)
		if err != nil {
			log.Printf("Warning: Failed to convert message %d: %v", messageCount, err)
			continue
		}

		batch = append(batch, m)

		// Insert batch when size reached
		if len(batch) >= batchSize {
			if err := insertBatch(session, schema, tableName, batch); err != nil {
				return fmt.Errorf("failed to insert batch: %w", err)
			}
			insertCount += len(batch)
			log.Printf("Inserted %d records (total: %d, consumed messages: %d)", len(batch), insertCount, messageCount)
			batch = make([]*Measurement, 0, batchSize)
		}

		if time.Since(lastLogTime) >= logInterval {
			elapsed := time.Since(lastLogTime).Seconds()
			delta := insertCount - lastLogInsert
			rate := float64(delta) / elapsed
			log.Printf("Throughput: %.2f records/s over %.1fs (total inserted: %d)", rate, elapsed, insertCount)
			lastLogTime = time.Now()
			lastLogInsert = insertCount
		}
	}

	// Insert remaining records
	if len(batch) > 0 {
		if err := insertBatch(session, schema, tableName, batch); err != nil {
			return fmt.Errorf("failed to insert final batch: %w", err)
		}
		insertCount += len(batch)
		log.Printf("Inserted %d records (total: %d, consumed messages: %d, time_since_start %.2fs)", len(batch), insertCount, messageCount, time.Since(startTime).Seconds())
	}

	duration := time.Since(startTime)
	throughput := float64(insertCount) / duration.Seconds()
	log.Printf("Consumption complete! Total records inserted: %d, Total messages consumed: %d", insertCount, messageCount)
	log.Printf("Performance: Duration=%.2fs, Throughput=%.2f records/s", duration.Seconds(), throughput)
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
