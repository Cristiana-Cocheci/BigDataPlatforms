package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	smallcsvFilePath          = "/data/2025-01-01_bme280_sensor_113.csv"
	csvFilePath               = "/data/2025-06-01_bme280.csv"
	defaultKafkaBrokers       = "kafka:29092"
	defaultKafkaTopic         = "bme280-measurements"
	defaultKafkaConsumerGroup = "bme280-consumer-group"
	defaultCassandraKeyspace  = "mysimbdp_weather"
	defaultCassandraHosts     = "cassandra1,cassandra2,cassandra3"
	defaultTenantConfigDir    = "./tenant_configs"
	csvFormatBME280Full       = "bme280_full"
	csvFormatDHT22Compact     = "dht22_compact"
	batchSize                 = 25    // for Cassandra inserts
	kafkaBatchSize            = 10000 // for Kafka producer
)

var (
	kafkaBrokers       = getEnv("KAFKA_BROKERS", defaultKafkaBrokers)
	kafkaTopic         = getEnv("KAFKA_TOPIC", defaultKafkaTopic)
	kafkaConsumerGroup = getEnv("KAFKA_CONSUMER_GROUP", defaultKafkaConsumerGroup)
	cassandraKeyspace  = getEnv("CASSANDRA_KEYSPACE", defaultCassandraKeyspace)
	cassandraHosts     = parseCSVList(getEnv("CASSANDRA_HOSTS", defaultCassandraHosts))
)

type MeasurementJSON struct {
	SensorID         int      `json:"sensor_id"`
	SensorType       string   `json:"sensor_type"`
	Location         *float32 `json:"location"`
	Lat              *float32 `json:"lat"`
	Lon              *float32 `json:"lon"`
	Day              string   `json:"day"`
	Timestamp        string   `json:"timestamp"`
	Pressure         *float32 `json:"pressure"`
	Altitude         *float32 `json:"altitude"`
	PressureSealevel *float32 `json:"pressure_sealevel"`
	Temperature      *float32 `json:"temperature"`
	Humidity         *float32 `json:"humidity"`
}

type Measurement struct {
	sensor_id         int
	sensor_type       string
	location          *float32
	lat               *float32
	lon               *float32
	day               string
	hour              int
	timestamp         string
	pressure          *float32
	altitude          *float32
	pressure_sealevel *float32
	temperature       *float32
	humidity          *float32
}

type TenantConfig struct {
	TenantID       string             `json:"tenant_id"`
	SchemaProfile  string             `json:"schema_profile"`
	TablePrefix    string             `json:"table_prefix"`
	CSVFormat      string             `json:"csv_format"`
	SourceCSV      string             `json:"source_csv"`
	SourceChunkDir string             `json:"source_chunk_dir"`
	Schema         TenantSchemaConfig `json:"schema"`
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
		normalizedTenantID = "tenant1"
	}

	configDir := getEnv("TENANT_CONFIG_DIR", defaultTenantConfigDir)
	configPath := filepath.Join(configDir, normalizedTenantID+".json")

	content, err := os.ReadFile(configPath)
	if err != nil {
		return TenantConfig{}, fmt.Errorf("failed to read tenant config %s: %w", configPath, err)
	}

	var config TenantConfig
	if err := json.Unmarshal(content, &config); err != nil {
		return TenantConfig{}, fmt.Errorf("failed to parse tenant config %s: %w", configPath, err)
	}

	config.applyDefaults(normalizedTenantID)
	return config, nil
}

func (config *TenantConfig) applyDefaults(tenantID string) {
	if strings.TrimSpace(config.TenantID) == "" {
		config.TenantID = tenantID
	}

	if strings.TrimSpace(config.SchemaProfile) == "" {
		config.SchemaProfile = "custom"
	}

	if strings.TrimSpace(config.TablePrefix) == "" {
		config.TablePrefix = "sensor_measurements"
	}

	if strings.TrimSpace(config.CSVFormat) == "" {
		config.CSVFormat = csvFormatBME280Full
	}

	if strings.TrimSpace(config.SourceCSV) == "" {
		config.SourceCSV = csvFilePath
	}

	if strings.TrimSpace(config.SourceChunkDir) == "" {
		config.SourceChunkDir = "/data/chunks"
	}

	if strings.TrimSpace(config.Schema.TableSuffixField) == "" {
		config.Schema.TableSuffixField = "sensor_type"
	}
}

// Shared helper functions
func parseFloat32(s string) *float32 {
	if s == "" || s == "NaN" || s == "nan" {
		return nil
	}
	val, err := strconv.ParseFloat(s, 32)
	if err != nil {
		return nil
	}
	f := float32(val)
	if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
		return nil
	}
	return &f
}

func parseInt(s string) int {
	if s == "" {
		return 0
	}
	val, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return val
}

func createDay(t string) string {
	// 2025-01-01T01:13:29
	day := strings.Split(t, "T")[0]
	return day
}

func extractHour(t string) int {
	// 2025-01-01T01:13:29 -> extract 01
	parts := strings.Split(t, "T")
	if len(parts) < 2 {
		return 0
	}
	timeParts := strings.Split(parts[1], ":")
	if len(timeParts) < 1 {
		return 0
	}
	hour, err := strconv.Atoi(timeParts[0])
	if err != nil {
		return 0
	}
	return hour
}

func jsonToMeasurement(mj *MeasurementJSON) (*Measurement, error) {
	t := createDay(mj.Timestamp)
	hour := extractHour(mj.Timestamp)

	m := &Measurement{
		sensor_id:         mj.SensorID,
		sensor_type:       mj.SensorType,
		location:          mj.Location,
		lat:               mj.Lat,
		lon:               mj.Lon,
		day:               t,
		hour:              hour,
		timestamp:         mj.Timestamp,
		pressure:          mj.Pressure,
		altitude:          mj.Altitude,
		pressure_sealevel: mj.PressureSealevel,
		temperature:       mj.Temperature,
		humidity:          mj.Humidity,
	}

	return m, nil
}
