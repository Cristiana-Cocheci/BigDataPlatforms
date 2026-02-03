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

func produceMessages() error {
	// Create Kafka writer
	w := kafka.NewWriter(kafka.WriterConfig{
		Brokers:      []string{kafkaBrokers},
		Topic:        kafkaTopic,
		Balancer:     &kafka.LeastBytes{},
		RequiredAcks: -1,
	})
	defer w.Close()

	log.Println("Kafka producer connected to", kafkaBrokers)

	// Open CSV file
	file, err := os.Open(csvFilePath)
	if err != nil {
		return fmt.Errorf("failed to open CSV: %w", err)
	}
	defer file.Close()

	log.Println("Started reading CSV file:", csvFilePath)

	scanner := bufio.NewScanner(file)
	lineCount := 0
	messageCount := 0

	// Skip header
	if scanner.Scan() {
		lineCount++
	}

	messages := make([]kafka.Message, 0, 100)

	// Process each line
	for scanner.Scan() {
		lineCount++
		line := scanner.Text()

		if line == "" {
			continue
		}

		m, err := parseMeasurement(line)
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
			Key:   []byte(strconv.Itoa(m.SensorID)),
			Value: data,
		})

		// Send batch to Kafka
		if len(messages) >= 100 {
			if err := w.WriteMessages(context.Background(), messages...); err != nil {
				return fmt.Errorf("failed to write messages: %w", err)
			}
			messageCount += len(messages)
			log.Printf("Produced %d messages (total: %d, processed lines: %d)", len(messages), messageCount, lineCount)
			messages = make([]kafka.Message, 0, 100)
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

	log.Printf("Production complete! Total messages produced: %d, Total lines processed: %d", messageCount, lineCount)
	return nil
}

func parseMeasurement(line string) (*MeasurementJSON, error) {
	fields := strings.Split(line, ";")
	if len(fields) < 11 {
		return nil, fmt.Errorf("invalid number of fields: %d", len(fields))
	}

	return &MeasurementJSON{
		SensorID:         parseInt(strings.TrimSpace(fields[0])),
		SensorType:       strings.TrimSpace(fields[1]),
		Location:         parseFloat32(strings.TrimSpace(fields[2])),
		Lat:              parseFloat32(strings.TrimSpace(fields[3])),
		Lon:              parseFloat32(strings.TrimSpace(fields[4])),
		Day:              createDay(strings.TrimSpace(fields[5])),
		Timestamp:        strings.TrimSpace(fields[5]),
		Pressure:         parseFloat32(strings.TrimSpace(fields[6])),
		Altitude:         parseFloat32(strings.TrimSpace(fields[7])),
		PressureSealevel: parseFloat32(strings.TrimSpace(fields[8])),
		Temperature:      parseFloat32(strings.TrimSpace(fields[9])),
		Humidity:         parseFloat32(strings.TrimSpace(fields[10])),
	}, nil
}

func main() {
	// Wait for Kafka to be ready
	log.Println("Waiting for Kafka to be ready...")
	time.Sleep(5 * time.Second)

	if err := produceMessages(); err != nil {
		log.Fatalf("Producer error: %v", err)
	}
}
