package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/gocql/gocql"
	"github.com/segmentio/kafka-go"
)

func insertBatch(session *gocql.Session, measurements []*Measurement) error {
	if len(measurements) == 0 {
		return nil
	}

	batch := session.NewBatch(gocql.LoggedBatch)

	for _, m := range measurements {
		batch.Query(
			"INSERT INTO mysimbdp_weather.sensor_measurements_by_sensor_day (sensor_id, sensor_type, location, lat, lon, day, timestamp, pressure, altitude, pressure_sealevel, temperature, humidity) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			m.sensor_id,
			m.sensor_type,
			m.location,
			m.lat,
			m.lon,
			m.day,
			m.timestamp,
			m.pressure,
			m.altitude,
			m.pressure_sealevel,
			m.temperature,
			m.humidity,
		)
	}

	return session.ExecuteBatch(batch)
}

func consumeMessages(session *gocql.Session) error {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        []string{kafkaBrokers},
		Topic:          kafkaTopic,
		GroupID:        kafkaConsumerGroup,
		CommitInterval: time.Second,
		MaxBytes:       10e6,
	})
	defer r.Close()

	log.Println("Kafka consumer connected, consuming from topic:", kafkaTopic)

	batch := make([]*Measurement, 0, batchSize)
	messageCount := 0
	insertCount := 0

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
			if err := insertBatch(session, batch); err != nil {
				return fmt.Errorf("failed to insert batch: %w", err)
			}
			insertCount += len(batch)
			log.Printf("Inserted %d records (total: %d, consumed messages: %d)", len(batch), insertCount, messageCount)
			batch = make([]*Measurement, 0, batchSize)
		}
	}

	// Insert remaining records
	if len(batch) > 0 {
		if err := insertBatch(session, batch); err != nil {
			return fmt.Errorf("failed to insert final batch: %w", err)
		}
		insertCount += len(batch)
		log.Printf("Inserted %d records (total: %d, consumed messages: %d)", len(batch), insertCount, messageCount)
	}

	log.Printf("Consumption complete! Total records inserted: %d, Total messages consumed: %d", insertCount, messageCount)
	return nil
}

func main() {
	// Create cluster
	cluster := gocql.NewCluster("cassandra1", "cassandra2", "cassandra3")
	cluster.Keyspace = cassandraKeyspace
	cluster.Consistency = gocql.Quorum

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
