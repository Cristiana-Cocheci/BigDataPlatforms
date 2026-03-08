package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

func getChunkFilePath(config TenantConfig) string {
	replicaStr := os.Getenv("CHUNK_NUM")
	defaultCSV := config.SourceCSV

	if replicaStr == "" {
		log.Printf("CHUNK_NUM not set, using tenant default CSV: %s", defaultCSV)
		return defaultCSV
	}

	replica, err := strconv.Atoi(replicaStr)
	if err != nil {
		log.Printf("Invalid CHUNK_NUM=%s, using tenant default CSV: %s", replicaStr, defaultCSV)
		return defaultCSV
	}

	chunkDir := strings.TrimRight(config.SourceChunkDir, "/")
	chunkPath := fmt.Sprintf("%s/chunk_%d.csv", chunkDir, replica)

	if _, err := os.Stat(chunkPath); os.IsNotExist(err) {
		log.Printf("Chunk not found: %s, using tenant default CSV: %s", chunkPath, defaultCSV)
		return defaultCSV
	}

	log.Printf("Producer replica %d using chunk %s", replica, chunkPath)
	return chunkPath
}

func produceMessages() error {
	startTime := time.Now()

	tenantConfig, err := loadTenantConfig(os.Getenv("TENANT_ID"))
	if err != nil {
		return err
	}

	log.Printf(
		"Loaded tenant config: tenant=%s format=%s source=%s chunk_dir=%s",
		tenantConfig.TenantID,
		tenantConfig.CSVFormat,
		tenantConfig.SourceCSV,
		tenantConfig.SourceChunkDir,
	)

	// Create Kafka writer
	w := kafka.NewWriter(kafka.WriterConfig{
		Brokers:      []string{kafkaBrokers},
		Topic:        kafkaTopic,
		Balancer:     &kafka.Hash{}, // Use hash balancer for key-based partitioning
		RequiredAcks: -1,
		MaxAttempts:  3,
	})
	defer w.Close()

	log.Println("Kafka producer connected to", kafkaBrokers)

	// Open CSV chunk file
	filePath := getChunkFilePath(tenantConfig)
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open CSV: %w", err)
	}
	defer file.Close()

	log.Println("Started reading CSV file:", filePath)

	scanner := bufio.NewScanner(file)
	lineCount := 0
	messageCount := 0

	// Skip header
	if scanner.Scan() {
		lineCount++
	}

	messages := make([]kafka.Message, 0, kafkaBatchSize)

	// Process each line
	for scanner.Scan() {
		lineCount++
		line := scanner.Text()

		if line == "" {
			continue
		}

		m, err := parseMeasurement(line, tenantConfig)
		if err != nil {
			log.Printf("Warning: Failed to parse line %d: %v", lineCount, err)
			continue
		}

		// Convert to JSON
		data, err := json.Marshal(m)
		if err != nil {
			log.Printf("Warning: Failed to marshal line %d: %v", lineCount, err)
			continue
		}

		messages = append(messages, kafka.Message{
			// Partition key for kafka is sensor_id, so that all measurements from the same sensor go to the same partition
			Key:   []byte(strconv.Itoa(m.SensorID)),
			Value: data,
		})

		// Send batch to Kafka
		if len(messages) >= kafkaBatchSize {
			if err := w.WriteMessages(context.Background(), messages...); err != nil {
				return fmt.Errorf("failed to write messages: %w", err)
			}
			messageCount += len(messages)
			log.Printf("Produced %d messages (total: %d, processed lines: %d)", len(messages), messageCount, lineCount)
			messages = make([]kafka.Message, 0, kafkaBatchSize)
		}
	}

	// Send remaining messages
	if len(messages) > 0 {
		if err := w.WriteMessages(context.Background(), messages...); err != nil {
			return fmt.Errorf("failed to write remaining messages: %w", err)
		}
		messageCount += len(messages)
		log.Printf("Produced %d messages (total: %d, processed lines: %d)", len(messages), messageCount, lineCount)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanner error: %w", err)
	}

	duration := time.Since(startTime)
	throughput := float64(messageCount) / duration.Seconds()
	log.Printf("Production complete! Total messages produced: %d, Total lines processed: %d", messageCount, lineCount)
	log.Printf("Performance: Duration=%.2fs, Throughput=%.2f msg/s", duration.Seconds(), throughput)
	return nil
}

func parseMeasurement(line string, tenantConfig TenantConfig) (*MeasurementJSON, error) {
	fields := strings.Split(line, ";")

	switch strings.ToLower(strings.TrimSpace(tenantConfig.CSVFormat)) {
	case csvFormatBME280Full:
		if len(fields) < 11 {
			return nil, fmt.Errorf("invalid number of fields for format=%s: %d", tenantConfig.CSVFormat, len(fields))
		}

		timestamp := strings.TrimSpace(fields[5])

		return &MeasurementJSON{
			SensorID:         parseInt(strings.TrimSpace(fields[0])),
			SensorType:       strings.TrimSpace(fields[1]),
			Location:         parseFloat32(strings.TrimSpace(fields[2])),
			Lat:              parseFloat32(strings.TrimSpace(fields[3])),
			Lon:              parseFloat32(strings.TrimSpace(fields[4])),
			Day:              createDay(timestamp),
			Hour:             extractHour(timestamp),
			Timestamp:        timestamp,
			Pressure:         parseFloat32(strings.TrimSpace(fields[6])),
			Altitude:         parseFloat32(strings.TrimSpace(fields[7])),
			PressureSealevel: parseFloat32(strings.TrimSpace(fields[8])),
			Temperature:      parseFloat32(strings.TrimSpace(fields[9])),
			Humidity:         parseFloat32(strings.TrimSpace(fields[10])),
		}, nil

	case csvFormatDHT22Compact:
		if len(fields) < 8 {
			return nil, fmt.Errorf("invalid number of fields for format=%s: %d", tenantConfig.CSVFormat, len(fields))
		}

		timestamp := strings.TrimSpace(fields[5])

		return &MeasurementJSON{
			SensorID:         parseInt(strings.TrimSpace(fields[0])),
			SensorType:       strings.TrimSpace(fields[1]),
			Location:         parseFloat32(strings.TrimSpace(fields[2])),
			Lat:              parseFloat32(strings.TrimSpace(fields[3])),
			Lon:              parseFloat32(strings.TrimSpace(fields[4])),
			Day:              createDay(timestamp),
			Hour:             extractHour(timestamp),
			Timestamp:        timestamp,
			Pressure:         nil,
			Altitude:         nil,
			PressureSealevel: nil,
			Temperature:      parseFloat32(strings.TrimSpace(fields[6])),
			Humidity:         parseFloat32(strings.TrimSpace(fields[7])),
		}, nil

	default:
		return nil, fmt.Errorf("unsupported csv_format=%s for tenant=%s", tenantConfig.CSVFormat, tenantConfig.TenantID)
	}
}

func main() {
	// Wait for Kafka to be ready
	log.Println("Waiting for Kafka to be ready...")
	time.Sleep(5 * time.Second)

	if err := produceMessages(); err != nil {
		log.Fatalf("Producer error: %v", err)
	}
}
