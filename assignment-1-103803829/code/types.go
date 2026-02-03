package main

import (
	"math"
	"strconv"
	"strings"
)

const (
	smallcsvFilePath   = "/data/2025-01-01_bme280_sensor_113.csv"
	csvFilePath        = "/data/2025-06_bme280.csv"
	mediumcsvFilePath  = "/data/2025-06-01_bme280.csv"
	kafkaBrokers       = "kafka:29092"
	kafkaTopic         = "bme280-measurements"
	kafkaConsumerGroup = "bme280-consumer-group"
	cassandraKeyspace  = "mysimbdp_weather"
	batchSize          = 25    // for Cassandra inserts
	kafkaBatchSize     = 10000 // for Kafka producer
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
	timestamp         string
	pressure          *float32
	altitude          *float32
	pressure_sealevel *float32
	temperature       *float32
	humidity          *float32
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

func jsonToMeasurement(mj *MeasurementJSON) (*Measurement, error) {
	t := createDay(mj.Timestamp)

	m := &Measurement{
		sensor_id:         mj.SensorID,
		sensor_type:       mj.SensorType,
		location:          mj.Location,
		lat:               mj.Lat,
		lon:               mj.Lon,
		day:               t,
		timestamp:         mj.Timestamp,
		pressure:          mj.Pressure,
		altitude:          mj.Altitude,
		pressure_sealevel: mj.PressureSealevel,
		temperature:       mj.Temperature,
		humidity:          mj.Humidity,
	}

	return m, nil
}
