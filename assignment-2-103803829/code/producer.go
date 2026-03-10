package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

func parseNonNegativeInt(raw string) (int, bool) {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed < 0 {
		return 0, false
	}

	return parsed, true
}

func sourceIdentity() string {
	if hostname := strings.TrimSpace(os.Getenv("HOSTNAME")); hostname != "" {
		return hostname
	}

	return fmt.Sprintf("pid-%d", os.Getpid())
}

func chunkPathForIndex(chunkDir string, index int) string {
	return fmt.Sprintf("%s/chunk_%d.csv", chunkDir, index)
}

func claimChunk(claimDir string, chunkIndex int, identity string) (bool, error) {
	claimPath := filepath.Join(claimDir, fmt.Sprintf("chunk_%d.claim", chunkIndex))
	f, err := os.OpenFile(claimPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}

		return false, err
	}
	defer f.Close()

	if _, err := fmt.Fprintln(f, identity); err != nil {
		return false, err
	}

	return true, nil
}

func findClaimedChunk(claimDir string, chunkCount int, identity string) int {
	for i := 0; i < chunkCount; i++ {
		claimPath := filepath.Join(claimDir, fmt.Sprintf("chunk_%d.claim", i))
		content, err := os.ReadFile(claimPath)
		if err != nil {
			continue
		}

		if strings.TrimSpace(string(content)) == identity {
			return i
		}
	}

	return -1
}

func autoAssignChunkFilePath(config TenantConfig) string {
	rawChunkCount := strings.TrimSpace(os.Getenv("SOURCE_NUM_CHUNKS"))
	if rawChunkCount == "" {
		return ""
	}

	chunkCount, ok := parseNonNegativeInt(rawChunkCount)
	if !ok || chunkCount < 1 {
		log.Printf("Invalid SOURCE_NUM_CHUNKS=%q, chunk auto-assignment disabled", rawChunkCount)
		return ""
	}

	chunkDir := strings.TrimRight(config.SourceChunkDir, "/")
	if chunkDir == "" {
		log.Printf("source_chunk_dir is empty for tenant=%s, chunk auto-assignment disabled", config.TenantID)
		return ""
	}

	claimDir := filepath.Join(chunkDir, ".source_claims")
	if err := os.MkdirAll(claimDir, 0o755); err != nil {
		log.Printf("Failed to create chunk claim directory=%s: %v", claimDir, err)
		return ""
	}

	identity := sourceIdentity()
	if chunkIndex := findClaimedChunk(claimDir, chunkCount, identity); chunkIndex >= 0 {
		chunkPath := chunkPathForIndex(chunkDir, chunkIndex)
		if _, err := os.Stat(chunkPath); err == nil {
			log.Printf("Producer identity=%s reusing claimed chunk %d from %s", identity, chunkIndex, chunkPath)
			return chunkPath
		}
	}

	for chunkIndex := 0; chunkIndex < chunkCount; chunkIndex++ {
		chunkPath := chunkPathForIndex(chunkDir, chunkIndex)
		if _, err := os.Stat(chunkPath); err != nil {
			continue
		}

		claimed, err := claimChunk(claimDir, chunkIndex, identity)
		if err != nil {
			log.Printf("Failed to claim chunk %d for identity=%s: %v", chunkIndex, identity, err)
			continue
		}

		if claimed {
			log.Printf("Producer identity=%s auto-assigned chunk %d: %s", identity, chunkIndex, chunkPath)
			return chunkPath
		}
	}

	log.Printf("No unclaimed chunk available for identity=%s with SOURCE_NUM_CHUNKS=%d", identity, chunkCount)
	return ""
}

func getChunkFilePath(config TenantConfig) string {
	replicaStr := os.Getenv("CHUNK_NUM")
	defaultCSV := config.SourceCSV

	if strings.TrimSpace(replicaStr) == "" {
		if autoChunkPath := autoAssignChunkFilePath(config); autoChunkPath != "" {
			return autoChunkPath
		}

		log.Printf("CHUNK_NUM not set (or no chunk claim available), using tenant default CSV: %s", defaultCSV)
		return defaultCSV
	}

	replica, ok := parseNonNegativeInt(replicaStr)
	if !ok {
		log.Printf("Invalid CHUNK_NUM=%s, using tenant default CSV: %s", replicaStr, defaultCSV)
		return defaultCSV
	}

	chunkDir := strings.TrimRight(config.SourceChunkDir, "/")
	chunkPath := chunkPathForIndex(chunkDir, replica)

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
