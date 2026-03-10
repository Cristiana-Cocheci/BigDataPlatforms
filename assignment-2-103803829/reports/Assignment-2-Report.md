# Assignment 2 Report

**AI Usage Disclosure**:
>I declare that I have not used AI for writing the assignment report\
>I declare that I have used VSCode Copilot for code generation.


## Part 1
1.
mysimbdp-messagingsystem - Apache Kafka
streamingestworker
mysimbdp-coredms
multi tenancy model

**Messaging system** : I used Apache Kafka for near real-time ingestion. 

**Why Kafka?** The protocol it uses is optimized for high throughput, low latency, and efficient message "batching", which is ideal for a giant dataset that needs to be delivered at once. It gives a lot of integrated features that makes parallelism possible, so that I am able to use to the maximum capacity my resources (since for the first assignment everything runs on my local computer). The Kafka partitioning feature ties nicely with the Cassandra integrated partitioning, so when a consumer tries to insert a batch I am guaranteed that all datasamples will be in the same partition. (Kafka is partitioned by sensor_id and Cassandra partitions data by hour and then by sensor_id).

The design is as follows: each tenant provides a csv big data file daily with weather measurment records. In my experiment **tenant1** uses BME280 sensor records and **tenant2** uses DHT22 sensor records, which have different formats. 

Both tenants can ingest independently by running separate Kafka brokers, with tenant-specific source files and parsing rules defined in tenant_configs/*.json.

In the current setup, each tenant can also use a different CSV structure and source file through tenant_configs/*.json.


```Example tenant1 data:```
|sensor_id|sensor_type|location|lat|lon|timestamp|pressure|altitude|pressure_sealevel|temperature|humidity|
|---|---|---|---|---|---|---|---|---|---|---|
|113|BME280|45999|48.808|9.182|2025-01-01T01:13:29|99112.75|||-1.80|100.00|


``Example tenant2 data:``
|sensor_id|sensor_type|location|lat|lon|timestamp|temperature|humidity|
|---|---|---|---|---|---|---|---|
|36474|DHT22|81266|53.248|-6.124|2025-06-01T00:00:00|13.00|99.90|


**Multi-tenancy model** : All tenants will share the same Cassandra cluster, where each tenant *X* has its own keyspace named *mysimbdp_tenantX*. This way, it is easy for mysimpbdp to add and remove tenants based on the principle of pay-per-use for the following reasons:
 - *Rapid Provisioning (Onboarding)*: Adding a new tenant consists in only a CREATE KEYSPACE command. Since the infrastructure (the Cassandra cluster) is already running, there is no need for new containers / virtual machines / new software for every new customer.
 - *Granular Resource Management*: Cassandra allows configurations at the keyspace level. You can use Replication Factors to offer different service tiers, like for example a *gold* tenant might pay more for a replication factor of 3, while a *bronze* tenant pays less for a replication factor of 1.
 - *Instant Decommissioning (Offboarding)*: If a tenant stops paying, you can remove their entire footprint—data, schemas,  using a DROP KEYSPACE command.
 - *Simplified Monitoring and Billing*: By separating data into keyspaces, you can easily track storage metrics per keyspace. This allows you to bill tenants accurately based on the actual disk space or throughput they consume.


2.
DESIGN AND IMPLEMENT
mysimbdp-streamingestmanager
 - start and stop streamingestworker om demand
 - invoke streamingestworker as a blackbox

explain what tenant has to do to develop streamingestworker

```mysimbdp-streamingestmanager``` : it is a control plane that orchestrates Docker Compose services for multiple tenants. It can start/stop tenant specific *streamingestworker*s. It does not do any ingesting itself.

The tenant topology is hardcoded in a tenant registry map:

```json
"tenant1": {
		TenantID:          "tenant1",
		Zookeeper:         "zookeeper-tenant1",
		Kafka:             "kafka-tenant1",
		WorkerService:     "tenant1-streamingestworker",
		SourceService:     "tenant1-source",
		KafkaTopic:        "bme280-measurements",
		CassandraKeyspace: "mysimbdp_tenant1",
		SchemaProfile:     "bme280",
	},
```

The manager parses flags and executes the following commands:
- start:
    - resolves tenant(s) 
    - number of workers
    - number of kafka partitions
    - prepares chunks from the original data (splitting for efficient producer reads according to the number of workers chosen)
    - creates tenant keyspace if not already available
    - ensures kafka topic is available
    - creates kafka topic partitions 
- stop:
    - stops and removes worker containers (kafka consumers)
    - if stop-source=true also stops and removes source (kafka producers)
    - if stop-broker=true also stops and removes kafka broker 
    - it does not drop keyspaces or data from the Cassandra cluster, it only handles ingestion workers
- status:
    - can specify which tenant status by --tenant flag
    - shows tenant's stack, magager, monitor
- listen-alerts:
    - runs a HTTP server that recieves alerts from ```streamingestmonitor```

The manager only runs compose commands, the tenant specific details are a blackbox for it. They are handled as environment variables, which are fetched from the tenant configuration files.

What does the tenant have to do for *streamingestworker*:
 - A tenant provides a configuration JSON file, that will be used by the default Kafka producer and consumer internal for mysimpbdp.

```json
{
  "tenant_id": "tenant1",
  "schema_profile": "bme280",
  "table_prefix": "sensor_measurements",
  "csv_format": "bme280_full",
  "source_csv": "/data/tenant1/2025-06-01_bme280.csv",
  "source_chunk_dir": "/data/tenant1/chunks",
  "schema": {
    "table_suffix_field": "sensor_type",
    "columns": [
      { "name": "sensor_id", "type": "int", "field": "sensor_id" },
      { "name": "sensor_type", "type": "text", "field": "sensor_type" },
      { "name": "location", "type": "float", "field": "location" },
      { "name": "lat", "type": "float", "field": "lat" },
      { "name": "lon", "type": "float", "field": "lon" },
      { "name": "day", "type": "text", "field": "day" },
      { "name": "hour", "type": "int", "field": "hour" },
      { "name": "timestamp", "type": "text", "field": "timestamp" },
      { "name": "pressure", "type": "float", "field": "pressure" },
      { "name": "altitude", "type": "float", "field": "altitude" },
      { "name": "pressure_sealevel", "type": "float", "field": "pressure_sealevel" },
      { "name": "temperature", "type": "float", "field": "temperature" },
      { "name": "humidity", "type": "float", "field": "humidity" }
    ],
    "primary_key": {
      "partition": ["day", "hour"],
      "clustering": ["sensor_id", "timestamp"]
    }
  }
}
```

3.
DEVELOP 2 streamingestworkers

performance of ingestion tests, failures and exceptions, under normal assumed loads

then for heavy loads but with a limited capability, under-provisoning of
streamingestworker due to the limitation of mysimbdp resources

4.
DESIGN
mysimbdp-streamingestmonitor

average ingestion time, total ingestion data size, and number of records

components, flows and the mechanism for reporting

```mysimbdp-streamingestmonitor```: is a HTTP service that recieves worker performanec reports and forwards alerts to ```streamingestmanager``` when necessary.

A report format would look like this:
```go
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
  IngestedBytesInWindow   int64   `json:"ingested_bytes_in_window"`
  IngestedMBInWindow      float64 `json:"ingested_mb_in_window"`
  IngestedMBPerSec        float64 `json:"ingested_mb_per_sec"`
  TotalIngestedBytes      int64   `json:"total_ingested_bytes"`
  TotalIngestedMB         float64 `json:"total_ingested_mb"`
	TotalInserted           int     `json:"total_inserted"`
	TotalConsumed           int     `json:"total_consumed"`
}
```

I also designed a cooldown system. When an alert is sent, then there is a coodown period before any other alert is sent to the manager, to prevent useless spamming.

```mysimbdp-streamingestmonitor``` has the following configuration:
- MONITOR_LISTEN_ADDR - default 8081
- MANAGER_ALERT_URL - default http://streamingestmanager:8082/alerts
- MONITOR_MIN_THROUGHPUT_RPS - default 300
- MONITOR_MAX_AVG_BATCH_INGEST_MS - default 250
- MONITOR_ALERT_COOLDOWN_SECONDS - default 30

```mysimbdp-streamingestmonitor``` has the following endpoints:
- GET /healthz : returns 200 ok for health checks
- POST /reports: reports worker performance:
    - tenant ID
    - worker ID
    - ThroughputRecordsPerSec
    - AvgBatchIngestMS
    - IngestedMBInWindow
    - TotalIngestedMB
    - WindowSeconds
    - RecordsInWindow
    - When there is a reason to alert the manager (small throughput and/or small average batch ingest), it first checks it is not in a cooldown period, in which case the alert is skipped. If it is not in a cooldown period, it sends the alert in json format via HTTP.


5.

The ```mysimbdp-streamingestmonitor``` is implemented in (streamingestmonitor.go)[../code/streamingestmonitor.go].
There, the function *evaluateThresholds* recieves the worker performance and returns a list of possible alert reasons. If the list is empty, then the workers are under normal parameters. The reasons that can be added to the list are : minimum ingestion throughput not met, exceeded average batch ingest.

As explained in the previous point, the ```mysimbdp-streamingestmonitor``` sends HTTP messages to ```streamingestmanager```.

```Demonstrate these features.```
All the logs can be further analysed in (code/benchmark_results)[code/benchmark_results].
Here is an extract from [log_streamingestmanager.txt](../code/benchmark_results/good_test_20260310_135346/log_streamingestmanager.txt)


```
streamingestmanager  | monitor alert received: tenant=tenant1 worker=c1370749f7a9 severity=warning reasons=throughput 0.00 rps below minimum 1000000.00 rps throughput=0.00 avg_batch_ms=0.00 window_mb=0.0000 total_mb=0.0000
streamingestmanager  | monitor alert received: tenant=tenant2 worker=dd578a255fa2 severity=warning reasons=throughput 0.00 rps below minimum 1000000.00 rps throughput=0.00 avg_batch_ms=0.00 window_mb=0.0000 total_mb=0.0000
streamingestmanager  | monitor alert received: tenant=tenant2 worker=4bdc9b2157d4 severity=warning reasons=throughput 0.00 rps below minimum 1000000.00 rps throughput=0.00 avg_batch_ms=0.00 window_mb=0.0000 total_mb=0.0000
streamingestmanager  | monitor alert received: tenant=tenant1 worker=719e254c2696 severity=warning reasons=throughput 4942.16 rps below minimum 1000000.00 rps throughput=4942.16 avg_batch_ms=4.79 window_mb=11.5822 total_mb=18.3187
```


And here is an extract of logs from [log_streamingestmonitor.txt](../code/benchmark_results/good_test_20260310_135346/log_streamingestmonitor.txt).
```
streamingestmonitor  | 2026/03/10 11:56:31 report received: tenant=tenant1 worker=c1370749f7a9 throughput=0.00 rps avg_batch_ms=0.00 window=10.0s records=0 window_mb=0.0000 total_mb=0.0000 mbps=0.0000
streamingestmonitor  | 2026/03/10 11:56:31 alert forwarded: tenant=tenant1 worker=c1370749f7a9 reasons=throughput 0.00 rps below minimum 1000000.00 rps
streamingestmonitor  | 2026/03/10 11:56:32 report received: tenant=tenant1 worker=719e254c2696 throughput=0.00 rps avg_batch_ms=0.00 window=10.2s records=0 window_mb=0.0000 total_mb=0.0000 mbps=0.0000
streamingestmonitor  | 2026/03/10 11:56:32 alert skipped due to cooldown: tenant=tenant1 worker=719e254c2696
streamingestmonitor  | 2026/03/10 11:56:32 report received: tenant=tenant1 worker=911abcb03e78 throughput=0.00 rps avg_batch_ms=0.00 window=10.4s records=0 window_mb=0.0000 total_mb=0.0000 mbps=0.0000
streamingestmonitor  | 2026/03/10 11:56:32 alert skipped due to cooldown: tenant=tenant1 worker=911abcb03e78
streamingestmonitor  | 2026/03/10 11:56:34 report received: tenant=tenant1 worker=7e4d8976b59f throughput=2.05 rps avg_batch_ms=2525.51 window=12.2s records=25 window_mb=0.0058 total_mb=0.0058 mbps=0.0005
streamingestmonitor  | 2026/03/10 11:56:34 alert skipped due to cooldown: tenant=tenant1 worker=7e4d8976b59f
st
```


## Part 2
1. 
DESIGN 
bronze to silver data pipelines

DESIGN a schema for a set of constraints for tenant service agreement that mysimbdp will support

2.
IMPLEMENT an instance of a silver pipeline. Explain design as a tenant.
tenant-caching-dir: local disk within the platform

3.
DESIGN AND IMPELMENT mysimbdp-batchmanager, which uses silverpipeline as a blackbox

4.

5.


## Part 3

1. FIGURE of Architecture
 Explain how a platform provider could know the amount of data ingested/processed and existing errors/performance for individual tenants.


2. new architecture for a different data sink

3. new architecture for monitoring quality of data

4. new architecture for multiple silverpipelines

5. improve silverpipeline



## Under-Provisioned Benchmark Results (Latest Run)

- Run directory: `code/benchmark_results/underprovisioned_20260308_105157`
- Finished at: `2026-03-08T08:55:44Z`
- Goal: stress ingestion with intentionally under-provisioned workers (`WORKERS=1`) while source producers send data continuously.

### Test configuration

- `TENANTS=tenant1 tenant2`
- `WORKERS=1`
- `TEST_DURATION_SECONDS=90`
- `PREPARE_CHUNKS=false`
- `RESET_STACK=true`
- `MIN_THROUGHPUT_RPS=1000000`

Source: `code/benchmark_results/underprovisioned_20260308_105157/test_config.env`

### Monitor and alert summary

- Reports received: `21`
- Alerts forwarded: `11`
- Alerts seen by manager: `11`
- Total ingested data size (MB): `N/A in this legacy run folder; available in new runs as total_ingested_mb`

Source: `code/benchmark_results/underprovisioned_20260308_105157/run_summary.env`

### Throughput and data-size summary (monitor)

- tenant1: `avg 4366.15 rps`, `min 217.22`, `max 5053.53`, `avg batch 5.42 ms`
- tenant2: `avg 4393.40 rps`, `min 0.00`, `max 5033.88`, `avg batch 4.26 ms`

With the new monitor metric pipeline, `monitor_throughput_by_tenant.csv` also includes:
- `avg_ingested_mb_per_report`
- `total_ingested_mb`

Source: `code/benchmark_results/underprovisioned_20260308_105157/monitor_throughput_by_tenant.csv`

### Cassandra end-state validation

- tenant1 inserted (exact via hour partition sum): `549706`
- tenant2 inserted (direct `COUNT(*)`): `425685`
- Sample rows are present for both tenants.

Sources:
- `code/benchmark_results/underprovisioned_20260308_105157/cassandra_counts_tenant1_by_hour.txt`
- `code/benchmark_results/underprovisioned_20260308_105157/cassandra_counts_tenant2.txt`
- `code/benchmark_results/underprovisioned_20260308_105157/cassandra_samples_tenant1.txt`
- `code/benchmark_results/underprovisioned_20260308_105157/cassandra_samples_tenant2.txt`

### Success-rate style processing metric

This benchmark folder does not include a direct `success_rate` field, so the practical processing fraction is computed as:

`inserted_in_cassandra / produced_to_kafka`

- tenant1 produced: `1160000`, inserted: `549706`, processed fraction: `47.39%`
- tenant2 produced: `2088110`, inserted: `425685`, processed fraction: `20.39%`
- combined processed fraction: `30.03%`

Notes:
- Lower fractions in this run mainly reflect intentional under-provisioning and limited runtime, leaving backlog in Kafka.
- Worker logs for this run show no insert errors.

### Chunk-size note

- In this latest run, chunk prep was disabled (`PREPARE_CHUNKS=false`), so initial chunk-size lines are not present in this run folder.
- Example run with chunk-size lines: `code/benchmark_results/underprovisioned_20260308_103528/manager_start_tenant1.txt`
- `Total rows: 2914834`
- `Number of chunks: 1`
- `Rows per chunk: 2914834`