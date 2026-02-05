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

func getTableName(sensorType string, day string) string {
	// Replace dashes with underscores for valid Cassandra table name
	safeDay := strings.ReplaceAll(day, "-", "_")
	return fmt.Sprintf("mysimbdp_weather.sensor_measurements_%s_%s", sensorType, safeDay)
}

func createTableIfNotExists(session *gocql.Session, sensorType string, day string) error {
	tableName := getTableName(sensorType, day)

	createQuery := fmt.Sprintf(`
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
			PRIMARY KEY ((hour), sensor_id, timestamp)
		)
	`, tableName)

	if err := session.Query(createQuery).Exec(); err != nil {
		return fmt.Errorf("failed to create table %s: %w", tableName, err)
	}

	log.Printf("Created table: %s", tableName)
	return nil
}

func insertBatch(session *gocql.Session, tableName string, measurements []*Measurement) error {
	if len(measurements) == 0 {
		return nil
	}

	// Use UnloggedBatch for better performance
	batch := session.NewBatch(gocql.UnloggedBatch)

	insertQuery := fmt.Sprintf(
		"INSERT INTO %s (sensor_id, sensor_type, location, lat, lon, day, hour, timestamp, pressure, altitude, pressure_sealevel, temperature, humidity) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		tableName,
	)

	for _, m := range measurements {
		batch.Query(
			insertQuery,
			m.sensor_id,
			m.sensor_type,
			m.location,
			m.lat,
			m.lon,
			m.day,
			m.hour,
			m.timestamp,
			m.pressure,
			m.altitude,
			m.pressure_sealevel,
			m.temperature,
			m.humidity,
		)
	}

	if err := session.ExecuteBatch(batch); err != nil {
		return fmt.Errorf("failed to insert batch into %s: %w", tableName, err)
	}

	return nil
}

func consumeMessages(session *gocql.Session) error {
	startTime := time.Now()
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
	tableName := getTableName(sensorType, day)

	log.Printf("Detected sensor_type=%s, day=%s, creating table: %s", sensorType, day, tableName)

	// Create table once at the beginning
	if err := createTableIfNotExists(session, sensorType, day); err != nil {
		return err
	}

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
			if err := insertBatch(session, tableName, batch); err != nil {
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
		if err := insertBatch(session, tableName, batch); err != nil {
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
	// Create cluster
	cluster := gocql.NewCluster("cassandra1", "cassandra2", "cassandra3")
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
