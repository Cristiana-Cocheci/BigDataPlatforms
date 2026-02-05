package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gocql/gocql"
)

// QueryResult holds the result of a count query
type QueryResult struct {
	Hour  int
	Count int64
	Error error
}

// splitHosts splits a comma-separated list of hosts
func splitHosts(hostStr string) []string {
	hosts := []string{}
	for _, h := range strings.Split(hostStr, ",") {
		h = strings.TrimSpace(h)
		if h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// connectToCassandra establishes a connection to the Cassandra cluster
func connectToCassandra(hosts []string) (*gocql.Session, error) {
	cluster := gocql.NewCluster(hosts...)
	cluster.Keyspace = "mysimbdp_weather"
	cluster.Consistency = gocql.LocalOne
	cluster.ConnectTimeout = 10 * time.Second
	cluster.Timeout = 30 * time.Second

	session, err := cluster.CreateSession()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Cassandra: %w", err)
	}

	return session, nil
}

// queryCountByHour queries the count of records for a specific hour
func queryCountByHour(session *gocql.Session, hour int, tableName string) (int64, error) {
	query := fmt.Sprintf(
		"SELECT COUNT(*) FROM %s WHERE hour = ?",
		tableName,
	)

	var count int64
	if err := session.Query(query, hour).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to query count for hour %d: %w", hour, err)
	}

	return count, nil
}

// runParallelQueries executes count queries for multiple hours in parallel
func runParallelQueries(session *gocql.Session, hours []int, tableName string, numWorkers int) []QueryResult {
	// Create a channel for tasks
	taskChan := make(chan int, len(hours))
	resultChan := make(chan QueryResult, len(hours))
	var wg sync.WaitGroup

	// Launch worker goroutines
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for hour := range taskChan {
				count, err := queryCountByHour(session, hour, tableName)
				resultChan <- QueryResult{
					Hour:  hour,
					Count: count,
					Error: err,
				}
			}
		}()
	}

	// Send tasks to workers
	go func() {
		for _, hour := range hours {
			taskChan <- hour
		}
		close(taskChan)
	}()

	// Wait for all workers to finish
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect results
	results := make([]QueryResult, 0, len(hours))
	for result := range resultChan {
		results = append(results, result)
	}

	return results
}

func main() {
	// Command-line flags
	sensorType := flag.String("sensor", "BME280", "Sensor type")
	day := flag.String("day", "2025_06_01", "Day in YYYY_MM_DD format")
	hour := flag.Int("hour", -1, "Specific hour to query (0-23). If not set, queries all hours.")
	workers := flag.Int("workers", 4, "Number of parallel workers")
	singleQuery := flag.Bool("single", false, "Run single query mode (query only the hour specified)")
	host := flag.String("host", "localhost:9042", "Cassandra host (use localhost:9042 from host, or cassandra1,cassandra2,cassandra3 from Docker)")

	flag.Parse()

	// Parse hosts
	hosts := []string{}
	if *host != "" {
		// Split by comma for multiple hosts
		for _, h := range splitHosts(*host) {
			hosts = append(hosts, h)
		}
	}

	// Connect to Cassandra
	session, err := connectToCassandra(hosts)
	if err != nil {
		log.Fatalf("Connection error: %v", err)
	}
	defer session.Close()

	tableName := fmt.Sprintf("mysimbdp_weather.sensor_measurements_%s_%s", *sensorType, *day)

	log.Printf("Connected to Cassandra. Table: %s\n", tableName)
	log.Printf("Using %d parallel workers\n", *workers)

	// Single query mode
	if *singleQuery || *hour >= 0 {
		if *hour < 0 || *hour > 23 {
			log.Fatalf("Hour must be between 0 and 23")
		}

		startTime := time.Now()
		count, err := queryCountByHour(session, *hour, tableName)
		elapsed := time.Since(startTime)

		if err != nil {
			log.Fatalf("Error: %v", err)
		}

		fmt.Printf("Hour %d: %d records (%.2f ms)\n", *hour, count, elapsed.Seconds()*1000)
		return
	}

	// Parallel query mode for all hours
	hours := make([]int, 24)
	for i := 0; i < 24; i++ {
		hours[i] = i
	}

	startTime := time.Now()
	results := runParallelQueries(session, hours, tableName, *workers)
	elapsed := time.Since(startTime)

	// Sort results by hour for display
	resultMap := make(map[int]QueryResult)
	for _, result := range results {
		resultMap[result.Hour] = result
	}

	totalCount := int64(0)
	errorCount := 0

	fmt.Printf("\n%-8s %-15s %-12s\n", "Hour", "Count", "Status")
	fmt.Println("-----------------------------------")

	for i := 0; i < 24; i++ {
		if result, ok := resultMap[i]; ok {
			if result.Error != nil {
				fmt.Printf("%-8d %-15s %-12s\n", result.Hour, "ERROR", result.Error.Error())
				errorCount++
			} else {
				fmt.Printf("%-8d %-15d %-12s\n", result.Hour, result.Count, "OK")
				totalCount += result.Count
			}
		}
	}

	fmt.Println("-----------------------------------")
	fmt.Printf("Total records: %d\n", totalCount)
	fmt.Printf("Total time: %.2f seconds\n", elapsed.Seconds())
	fmt.Printf("Errors: %d\n", errorCount)

	if errorCount > 0 {
		os.Exit(1)
	}
}
