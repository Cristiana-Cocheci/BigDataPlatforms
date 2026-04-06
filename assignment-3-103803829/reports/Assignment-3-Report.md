# Assignment 2 Report

**AI Usage Disclosure**:
>I declare that I have not used AI for writing the assignment report\
>I declare that I have used VSCode Copilot for code generation.


## Part 1
### 1.
As the **raw dataset** I select the weather sensor data. The dataset is time-ordered, high-frequency sensor telemetry, which fits the sensor data qualities naturally. 

```Example data:```
|sensor_id|sensor_type|location|lat|lon|timestamp|temperature|humidity|
|---|---|---|---|---|---|---|---|
|36474|DHT22|81266|53.248|-6.124|2025-06-01T00:00:00|13.00|99.90|

The **analytics scenario** is the following: The sensor data comes from an indoor facility - the goal is to detect sudden temperature or humidity anolmalies that may indicate hazardz for the facility, or a sensor defect. Streaming is crutial in this scenario, because analysis is useful only on limited time windows, not after batch processing at the end of the day. To mitigate a risk as soon as possible, the data needs to be processed onine.

The data is suitable for this task because it provides the continuous timestamp observations and temperature and humidity that can be used in real time. Moreover, the data can be partitioned by sensor and time for scalable ingestion and processing.

**streamanalyticsapp** will receive a raw measurement event stream, will analyse data in windows of 15 minutes, with updates every minute. It will compute :
- min temperature / humidity
- max temperature / humidity
- mean temperature / humidity
- median temperature / humidity
- number of missing minutes (to asses sensor functionality)

The output will be an aggregated report record of the following schema:

|sensor_id|sensor_type|location|lat|lon|start_timestamp|end_timestamp|t_min|t_max|t_median|t_avg|h_min|h_max|h_median|h_avg|missing_min|is_alert|
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
|36474|DHT22|81266|53.248|-6.124|2025-06-01T00:00:00|2025-06-01T00:00:15|13.00|13.00|13.00|13.00|99.90|99.90|99.90|99.90|0|0

The reports will be stored in a database in Cassandra, corresponding to the tenant keyspace. If the event has the field **is_alert** set to 1 (True), a row will be added in the alert table of the tenant. Partitioning will be done by day and hour.



I will be using as testing data a single day of the dht22 sensor data. It can be found in (2025-06-01_dht22.csv)[] **TO DO: ADD PATH HERE**.

### 2.

i) In **streamanalyticsapp**, the data streams should be keyed because:
- **the metrics are dependent on the sensor id**, we should treat differently measurements from different sensors; Doing that enables us to detect failures in sensors and to pinpoint sources of temperature/ humidity abnormalities to a specific location per a time window.
- for scalability reasons, having independent keyed data per sensor makes the system scale organically with the number of sensors

The key would be **sensor_id**. Each tenant would have a different kafka topic, so the data would be implicitly separated by tenant.

ii) The data is sensitive to a single datapoint -> we do not want to lose messages.
However, having duplicates does not necessarily damage the analysis, if it is treated correctly (checking for timestamp duplicates). Since I am using Kafka, the message delivery should satisfy **at-least-once**.

### 3.
i) The actual event time (the moment the sensor produced the measurement) should be associated with stream data for analitics. This makes sure we detect the anomaly in the right timeframe, no matter the delay on the pipeline (for example inside Kafka messaging or Flink processig). The data already has these timestamps associated with it.

ii) I am going to use sliding windows for trend detection. The window length should be 15 minutes, and slide each minute (a new window record is computed every minute, by kicking out latest minute and adding newest minute). The measurements are specified in the first exercise as the report example (min temp, max temp, avg temp, median temp ...).

iii) Out-of-order data could happen because of :
- network lag between sensor and broker
- kafka retries sending message after error occurs
- container or service breaks and restarts, causing a delay in messages sent to that partition
- sensor buffers multiple messages before sending them

iv) Watermarks are needed to accomodate potential out-of-order events. Especially because I am using sliding windows, watermarks are a necessity. Without them, windows may close too early, causing potential data loss. For example, we could wait 3 minutes for out-of-order data to appear before finalizing results. This is a decent timeframe, since we excpect messages once per minute. For windows of 15 minutes it is a short delay, but long enough to accomodate any reasonable lag (caused by buffering/ network malfunctions...)

### 4. 
Some key performance metrics are:
- throughput : 
    - number of events processed / second; 
    - I would monitor Kafka topic ingestion rate and Flink processing rate with the help of built-in metrics (numRecordsInPerSecond, numRecordsOutPerSecond)
    - it is important because through it a system administrator can evaluate performance and scalability, and asses if the pipeline works as expected under heavier loads
- end-to-end latency: 
    - time between event timestamp (when sensor generates message) and result is produced (15min window report sent to the database)
    - it will be tracked in the code of the system: when a window aggregated message is ready we measure processing timestamp (the final time), and substract from it the event time in the raw data record
    - it is relevant for real time anomaly detection - we can see how much time an event spends inside the pipeline and address potential performance issues; 
- processing delay:
    - time inside Flink processing system (from ingestion to result)
    - I can use Flink internal metrics
    - helps identify slow operations and bottlenecks
- missin data rate:
    - proportion of actual datapoints inside a window to the expected datapoints (number of messages / 15 - because we expect 15 messages)
    - we get them by counting number of different events that were received in the window
    - detect sensor failures, useful for tenants to manage their sensors inside their facilities
- duplicate event rate
    - how many duplicates we have per total number of messages in a window
    - detect via the key (sensor_id, timestamp) inside Flink
    - important for ensuring correct analysis


### 5.
![Design](../code/figures/ass3.drawio.png)

**Ingestion** is being done with Kafka. The system design is mostly the same as in assignment 2. Kafka will ingest CSV files and transform them into event messages. There will be one topic per tenant and the partitioning key will be sensor_id.

Why Kafka? High throughput, fault tolerant, supports at-least-once delivery... Reasons explained in detail in previous assignment.

The streaming engine chosen for **streamanalyticsapp** is Apache Flink. It will process timed events in sliding windows, aggregate results and form reports. Reports are always sent to Cassandra to the reports table. Alerts are also always sent to Cassandra to the alerts table. Alerts can also be instantly forwarded to the tenant thorugh a http message service, for example.

Why Flink? It has built-in support for timed events, watermarks and sliding windows. It also provides a low-latency processing.

The **mysimbdp-coredms** remains Cassandra, same system design as in previous assignments. It will store data in two tables / tenant keyspace / sensor type (dht22):
- aggregated window results
- alerts



## PART 2.


#### 1. 

The same Kafka messaging system as in the previous assignments is used. The dedicated producer script, kafka_csv_producer.py:7 **TODO ADD REF**, reads the CSV row by row, validates the input schema, converts each row to JSON, and publishes it to Kafka.

i) Raw sensor data comes from the DHT22 CSV file 2025-06-01_dht22.csv. **TODO ADD REFERENCE**

**INPUT DATA EXAMPLE:**
|sensor_id|sensor_type|location|lat|lon|start_timestamp|end_timestamp|t_min|t_max|t_median|t_avg|h_min|h_max|h_median|h_avg|missing_min|is_alert|
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
|36474|DHT22|81266|53.248|-6.124|2025-06-01T00:00:00|2025-06-01T00:00:15|13.00|13.00|13.00|13.00|99.90|99.90|99.90|99.90|0|0

**OUTPUT DATA** is inserted in a table of the following format:

```sql
CREATE TABLE IF NOT EXISTS stream_analytics_results (
                day text,
                hour int,
                sensor_id int,
                window_start timestamp,
                tenant_id text,
                sensor_type text,
                location text,
                lat float,
                lon float,
                window_end timestamp,
                t_min float,
                t_max float,
                t_median float,
                t_avg float,
                h_min float,
                h_max float,
                h_median float,
                h_avg float,
                missing_min int,
                is_alert boolean,
                records_in_window int,
                PRIMARY KEY ((day, hour), sensor_id, window_start)
            )
```

**Why enforce both schemas**:

- Input schema enforcement protects event-time processing from malformed rows (missing `timestamp`, non-numeric metrics, etc.).
- Output schema enforcement gives a stable contract for tenant-side consumers (dashboard/alert endpoint).
- Without strict schemas, window computations can mix bad values and produce unreliable alerts.

ii) The kafka producer uses:
- sensor_id as the Kafka key
- a JSON object as the Kafka value

The JSON object is expected by the **streamanalyticsapp** in the following format:
```JSON
{sensor_id, sensor_type, location, lat, lon, timestamp, temperature, humidity}
```
Then, the **streamanalyticsapp** converts types into int/float/string/time. Then returns normalized EventTuple for Flink processing.

```py
# Input event tuple schema:
# (sensor_id, sensor_type, location, lat, lon, event_ts_ms, temperature, humidity)
EventTuple = Tuple[int, str, str, float, float, int, float, float]
```

After flink processing, the new JSON will look like the **OUTPUT DATA FORMAT** (example in previous point i)).

iii) The logic is implemented inside **AnalyticsWindowFunction** in **streamanalyticsapp**. Here there re received keyed windows of EventTuples.

From a window of EventTuples:
- extract static sensor data from first record (sensor_id, sensor_type, lat, lon)
- build temperature and humidity arrays
- compute min/max/avg/median for the measurements
-build alerts for missing data / threshold breaches
- emit a compact JSON result (example below)

```py
result = {
            "tenant_id": self.tenant_id,
            "sensor_id": sensor_id,
            "sensor_type": sensor_type,
            "location": location,
            "lat": lat,
            "lon": lon,
            "window_start": ms_to_iso(context.window().start),
            "window_end": ms_to_iso(context.window().end),
            "t_min": round(t_min, 3),
            "t_max": round(t_max, 3),
            "t_median": round(t_median, 3),
            "t_avg": round(t_avg, 3),
            "h_min": round(h_min, 3),
            "h_max": round(h_max, 3),
            "h_median": round(h_median, 3),
            "h_avg": round(h_avg, 3),
            "missing_min": missing_min,
            "is_alert": bool(is_alert),
            "records_in_window": len(records),
        }
```

Configurable thresholds:

- `TEMP_ALERT_LOW`, `TEMP_ALERT_HIGH`, `HUM_ALERT_LOW`, `HUM_ALERT_HIGH` are runtime environment variables.

iv) 
The callback is sent where there is an alert (is_alert flag is true). An alert appears when:
- the temperature/humidity aggregated values are not in required parameters (set previously by the tenant)
- there are missing messages

The tenant provides a HTTP address (**callback_url**), where the results are sent via HTTP POST to the tenant:
```py
req = request.Request(
        self.callback_url,
        data=payload.encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
```

<!-- It can be bounded with MAX_EVENTS for repeatable tests, and it skips malformed rows instead of failing the whole run. -->
<!-- The core parsing logic is in kafka_csv_producer.py:32 and the Kafka send logic is in kafka_csv_producer.py:67. -->

### 2. 
The test environment I created for streamanalyticsapp is a single-tenant tenant2 setup that emulates a streaming pipeline end to end.

I emulate streaming data by replaying a static CSV as timed Kafka events. To control emulation i can use the following parameters:
- EMIT_DELAY_MS : make live ingestion faster or slower 
- MAX_EVENTS : bounds test event number

The flow is:
- -> Input CSV file
- -> Emulated Ingestion (Kafka Messaging System)
- -> Streaming Engine/ Stream Analytics App (Flink)
    - -> Optional alerts sent live to tenant (callbacks via HTTP)
    - -> Analytics results inserted in Cassandra database (table name: *stream_analytics_results*)
    - -> Alerts inserted in Cassandra database (table name: *stream_analytics_alerts*)

What Flink does here:
- it recieves events
- it handles out-of-order events via watermark strategy with bounded out-of-orderness controlled by parameter OUT_OF_ORDER_MINUTES
- it groups events by sensor id
- applies a sliding window to events (15min, sliding every minute)
- computes analytics in each window
- computes output and sends it to appropriate sink

The environment is defined in Docker Compose for a single tenant pipeline (same as in previous assignments):
- ZooKeeper: zookeeper-tenant2
- Kafka broker: kafka-tenant2
- Kafka topic initialization: kafka-topic-init-tenant2
- Cassandra nodes: cassandra1, cassandra2, cassandra3
- Cassandra keyspace initialization:
    - cassandra-init-tenant2

Kafka is configured so the app can talk to it both inside Docker and from the host machine:
- internal listener: kafka-tenant2:29092
- host listener: localhost:9094

<!-- The relevant Kafka listener config is in docker-compose.yml:30 and docker-compose.yml:31. -->

Cassandra is configured to use the tenant2 keyspace **mysimbdp_tenant2**, which is also the default in the **streamanalyticsapp**.
The app also reads Cassandra hosts from CASSANDRA_HOSTS, with the default host being 127.0.0.1.


### 3.

### 4.

### 5.


## Part 3
### 1.

### 2.

### 3.

### 4.

### 5.






##### HELLO WHAT 

## Part 2
### 1.

i) 
Input record schema:

The Kafka message is expected to represent one sensor reading with these fields:
sensor_id
sensor_type
location
lat
lon
timestamp
temperature
humidity
In code, this is represented as the EventTuple type:
streamanalyticsapp.py:24
The Kafka parsing logic enforces the same schema before the Flink job accepts a record:
streamanalyticsapp.py:269
Analytics output schema:
Each analytics result contains:
tenant_id
sensor_id
sensor_type
location
lat
lon
window_start
window_end
t_min, t_max, t_median, t_avg
h_min, h_max, h_median, h_avg
missing_min
is_alert
records_in_window
This is built in the window function result object:
streamanalyticsapp.py:125
The same output is persisted to Cassandra in a fixed table schema:
streamanalyticsapp.py:173
Why the schema is important:

It makes the pipeline predictable end to end: producer, Flink job, callback, and Cassandra all agree on field names and types.
It lets the app validate and reject malformed rows early instead of letting bad data corrupt window calculations or storage.
It ensures consistent analytics results, which is essential for querying and comparing windows later.
It also makes serialization/deserialization deterministic, since the app knows exactly which fields it must read and which fields it will emit.
hy enforce it:

Input enforcement prevents missing columns, empty values, and type conversion failures.
Output enforcement keeps the analytics result stable and insertable into Cassandra without runtime ambiguity.
The producer enforces the input schema before publishing:
kafka_csv_producer.py:32
The Flink app enforces the schema again after consuming Kafka:

ii)

(ii) Data serialization/deserialization in streamanalyticsapp

Producer side:

CSV rows are parsed into a Python dictionary.
That dictionary is serialized to JSON with json.dumps(...).
The JSON bytes are then sent to Kafka as the message value.
This happens at:
kafka_csv_producer.py:93
The producer uses Kafka message keys too, with sensor_id as the key:
kafka_csv_producer.py:67
Consumer side in streamanalyticsapp:

Kafka message values are read as bytes.
They are decoded from UTF-8.
The decoded JSON string is deserialized with json.loads(...).
The parsed dictionary is then converted into the internal EventTuple.
This logic is here:
streamanalyticsapp.py:291
Output serialization:

The window function turns the aggregated result dictionary into a compact JSON string with json.dumps(..., separators=(",", ":")).
That JSON string is then:
printed to stdout,
appended to the analytics log file,
written to Cassandra,
and optionally sent to the tenant callback endpoint.
Result creation:
streamanalyticsapp.py:125
Logging and downstream delivery:
streamanalyticsapp.py:149
So the data flow is effectively:

CSV row -> Python dict -> JSON bytes in Kafka -> JSON string in Flink -> Python dict for analytics -> JSON output again for callback/logging

iii)
(iii) Logic of the processing functions in streamanalyticsapp

The main processing path is:

parse_kafka_record(...)
Validates required Kafka fields.
Converts string values into the expected types.
Converts the timestamp into event-time milliseconds.
Returns the normalized EventTuple.
Location:
streamanalyticsapp.py:269
load_events_from_kafka(...)
Opens a Kafka consumer with the configured broker, topic, and group.
Polls messages until either:
enough valid events are read, or
the idle timeout expires.
Skips malformed messages and logs warnings.
Fails if no valid events are consumed.
Location:
streamanalyticsapp.py:291
SensorKeySelector
Uses sensor_id as the grouping key.
This means each sensor is windowed independently.
Location:
streamanalyticsapp.py:65
EventTimestampAssigner
Uses the event’s timestamp field as event time.
This is what enables event-time windowing instead of processing-time windowing.
Location:
streamanalyticsapp.py:70
AnalyticsWindowFunction
Runs a 15-minute sliding event-time window every 1 minute.
For each sensor window, it computes:
min, max, average, median for temperature and humidity
number of missing minute buckets
alert flag if values are out of bounds or data is incomplete
It then produces one JSON analytics record.
Location:
streamanalyticsapp.py:81
Output object creation:
streamanalyticsapp.py:125
CallbackMapFunction
Receives each JSON analytics result.
Appends it to the analytics output log.
Converts the JSON back to a record and inserts it into Cassandra.
If a callback URL is configured, it sends the same JSON to the tenant via HTTP POST.
Locations:
class definition: streamanalyticsapp.py:149
Cassandra table creation: streamanalyticsapp.py:173
main()
Reads configuration from environment variables.
Builds the Flink stream.
Applies timestamps, watermarks, keying, and sliding windows.
Sends the final analytics stream to the callback/Cassandra sink.
The callback is only active if TENANT_CALLBACK_URL is set:
streamanalyticsapp.py:372
The idle timeout and bounded Kafka read are controlled here:
streamanalyticsapp.py:370
The final sink stage is here:
streamanalyticsapp.py:434


iv)

(iv) When and how results are sent back to the tenant near real time

Conditions required:

TENANT_CALLBACK_URL must be set to a non-empty URL.
The Flink app must be running with the Kafka and Cassandra settings configured.
In the runbook, the callback demo is enabled like this:
how_to_run_streamanalyticsapp.txt:24
how_to_run_streamanalyticsapp.txt:30
The runbook also documents the relevant tenant callback variable:
how_to_run_streamanalyticsapp.txt:51
How the results are sent:

After each window result is computed, the app serializes it to JSON.
The callback sink sends that JSON as an HTTP POST body.
This happens inside CallbackMapFunction.map(...):
streamanalyticsapp.py:199
The POST uses:
Content-Type: application/json
a short timeout of 2 seconds
That means the tenant gets the result immediately after the analytics window emits it, rather than waiting for a batch job to finish.
Why this is near real time:

The job uses event-time sliding windows of 15 minutes with a 1-minute slide, so new results are produced continuously as the stream advances.
Out-of-order data is tolerated with bounded lateness via OUT_OF_ORDER_MINUTES.
The consumer loop also uses KAFKA_IDLE_TIMEOUT_MS and MAX_EVENTS so the current implementation is bounded and practical for a single-run demo.
Relevant config is in main():
streamanalyticsapp.py:370
streamanalyticsapp.py:372
Important nuance:

In this implementation, the “near real-time” tenant push is the optional HTTP callback path, not Cassandra alone.
Cassandra is the persistent sink.
The callback is the immediate delivery path to the tenant endpoint, and it is only active when TENANT_CALLBACK_URL is configured.



### 2.

The test environment I created for streamanalyticsapp is a single-tenant tenant2 setup that emulates a streaming pipeline end to end:

1. 

The same Kafka messaging system as in the previous assignments is used. The dedicated producer script, kafka_csv_producer.py:7 **TODO ADD REF**, reads the CSV row by row, validates the input schema, converts each row to JSON, and publishes it to Kafka.

i) Raw sensor data comes from the DHT22 CSV file 2025-06-01_dht22.csv. **TODO ADD REFERENCE**

**INPUT DATA EXAMPLE:**
|sensor_id|sensor_type|location|lat|lon|start_timestamp|end_timestamp|t_min|t_max|t_median|t_avg|h_min|h_max|h_median|h_avg|missing_min|is_alert|
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
|36474|DHT22|81266|53.248|-6.124|2025-06-01T00:00:00|2025-06-01T00:00:15|13.00|13.00|13.00|13.00|99.90|99.90|99.90|99.90|0|0

**OUTPUT DATA** is inserted in a table of the following format:

```sql
CREATE TABLE IF NOT EXISTS stream_analytics_results (
                day text,
                hour int,
                sensor_id int,
                window_start timestamp,
                tenant_id text,
                sensor_type text,
                location text,
                lat float,
                lon float,
                window_end timestamp,
                t_min float,
                t_max float,
                t_median float,
                t_avg float,
                h_min float,
                h_max float,
                h_median float,
                h_avg float,
                missing_min int,
                is_alert boolean,
                records_in_window int,
                PRIMARY KEY ((day, hour), sensor_id, window_start)
            )
```

ii) The kafka producer uses:
- sensor_id as the Kafka key
- a JSON object as the Kafka value

The JSON object is expected by the **streamanalyticsapp** in the following format:
```JSON
{sensor_id, sensor_type, location, lat, lon, timestamp, temperature, humidity}
```
Then, the **streamanalyticsapp** converts types into int/float/string/time. Then returns normalized EventTuple for Flink processing.

```py
# Input event tuple schema:
# (sensor_id, sensor_type, location, lat, lon, event_ts_ms, temperature, humidity)
EventTuple = Tuple[int, str, str, float, float, int, float, float]
```

After flink processing, the new JSON will look like the **OUTPUT DATA FORMAT** (example in previous point i)).

iii) The logic is implemented inside **AnalyticsWindowFunction** in **streamanalyticsapp**. Here there re received keyed windows of EventTuples.

From a window of EventTuples:
- extract static sensor data from first record (sensor_id, sensor_type, lat, lon)
- build temperature and humidity arrays
- compute min/max/avg/median for the measurements
-build alerts for missing data / threshold breaches
- emit a compact JSON result (example below)

```py
result = {
            "tenant_id": self.tenant_id,
            "sensor_id": sensor_id,
            "sensor_type": sensor_type,
            "location": location,
            "lat": lat,
            "lon": lon,
            "window_start": ms_to_iso(context.window().start),
            "window_end": ms_to_iso(context.window().end),
            "t_min": round(t_min, 3),
            "t_max": round(t_max, 3),
            "t_median": round(t_median, 3),
            "t_avg": round(t_avg, 3),
            "h_min": round(h_min, 3),
            "h_max": round(h_max, 3),
            "h_median": round(h_median, 3),
            "h_avg": round(h_avg, 3),
            "missing_min": missing_min,
            "is_alert": bool(is_alert),
            "records_in_window": len(records),
        }
```

iv) 
The callback is sent where there is an alert (is_alert flag is true). An alert appears when:
- the temperature/humidity aggregated values are not in required parameters (set previously by the tenant)
- there are missing messages

The tenant provides a HTTP address (**callback_url**), where the results are sent via HTTP POST to the tenant:
```py
req = request.Request(
        self.callback_url,
        data=payload.encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
```

<!-- It can be bounded with MAX_EVENTS for repeatable tests, and it skips malformed rows instead of failing the whole run. -->
<!-- The core parsing logic is in kafka_csv_producer.py:32 and the Kafka send logic is in kafka_csv_producer.py:67. -->

2. I emulate streaming data by replaying a static CSV as timed Kafka events. To control emulation i can use the following parameters:
- EMIT_DELAY_MS : make live ingestion faster or slower 
- MAX_EVENTS : bounds test event number

The flow is:
- -> Input CSV file
- -> Emulated Ingestion (Kafka Messaging System)
- -> Streaming Engine/ Stream Analytics App (Flink)
    - -> Optional alerts sent live to tenant (callbacks via HTTP)
    - -> Analytics results inserted in Cassandra database (table name: *stream_analytics_results*)
    - -> Alerts inserted in Cassandra database (table name: *stream_analytics_alerts*)

What Flink does here:
- it recieves events
- it handles out-of-order events via watermark strategy with bounded out-of-orderness controlled by parameter OUT_OF_ORDER_MINUTES
- it groups events by sensor id
- applies a sliding window to events (15min, sliding every minute)
- computes analytics in each window
- computes output and sends it to appropriate sink

The environment is defined in Docker Compose for a single tenant pipeline (same as in previous assignments):
- ZooKeeper: zookeeper-tenant2
- Kafka broker: kafka-tenant2
- Kafka topic initialization: kafka-topic-init-tenant2
- Cassandra nodes: cassandra1, cassandra2, cassandra3
- Cassandra keyspace initialization:
    - cassandra-init-tenant2

Kafka is configured so the app can talk to it both inside Docker and from the host machine:
- internal listener: kafka-tenant2:29092
- host listener: localhost:9094

<!-- The relevant Kafka listener config is in docker-compose.yml:30 and docker-compose.yml:31. -->

Cassandra is configured to use the tenant2 keyspace **mysimbdp_tenant2**, which is also the default in the **streamanalyticsapp**.
The app also reads Cassandra hosts from CASSANDRA_HOSTS, with the default host being 127.0.0.1.


3. How streamanalyticsapp is tested

The Flink app consumes the Kafka topic, normalizes each record into an internal tuple schema, applies event-time windowing, and writes results to Cassandra.
The main analytics window is a 15-minute sliding window with a 1-minute slide:
streamanalyticsapp.py:427
For test runs, I used a bounded configuration so the job finishes and is easy to verify:
KAFKA_BROKERS=127.0.0.1:9094
KAFKA_TOPIC=dht22-measurements
KAFKA_CONSUMER_GROUP=tenant2-flink-analytics
MAX_EVENTS=400
KAFKA_IDLE_TIMEOUT_MS=30000
CASSANDRA_HOSTS=127.0.0.1
CASSANDRA_KEYSPACE=mysimbdp_tenant2
Those parameters are documented in the run guide:
how_to_run_streamanalyticsapp.txt:24
how_to_run_streamanalyticsapp.txt:44
4. Other relevant test parameters

Python runtime: Python 3.11 virtual environment, because PyFlink does not work reliably on the newer system Python.
Java runtime: OpenJDK 11, required for PyFlink.
Logging:
app logs go to logs/streamanalyticsapp/app.log
analytics JSON output goes to logs/streamanalyticsapp/analytics_output.jsonl
producer logs go to logs/kafka_producer/producer.log
Optional near-real-time tenant callback:
set TENANT_CALLBACK_URL=http://127.0.0.1:8080/tenant2/analytics
start the local receiver with local_callback_receiver.py:53
The local receiver is just for testing the near-real-time push path; Cassandra remains the persistent sink.
5. Summary of the test environment
Input emulation: CSV -> JSON -> Kafka
Processing: Kafka -> Flink event-time sliding windows
Output: JSON analytics -> Cassandra, plus optional HTTP callback to tenant
Tenant scope: single tenant only, tenant2
Data target: keyspace mysimbdp_tenant2


### 3.

What the test environment does

The producer reads the DHT22 CSV and publishes JSON records to Kafka.
The stream app consumes those Kafka messages, parses them into a typed event tuple, computes sliding-window analytics, and writes results to Cassandra.
In the validated baseline run, the producer published 400 records, the app consumed 400 Kafka events, and Cassandra ended up with 5985 analytics rows. That row count is larger than the input count because each event participates in multiple overlapping sliding windows.
Observed operation
Producer stage:
CSV rows are converted to JSON and sent to the topic dht22-measurements.
Malformed source rows are skipped with warnings.
Stream stage:
The app logs startup, consumes the bounded Kafka batch, and emits one JSON analytics result per windowed sensor group.
Results are also inserted into Cassandra.
Sink stage:
Cassandra is used as the persistent result store.
If TENANT_CALLBACK_URL is set, the same JSON result is also POSTed back to the tenant endpoint.
(i) Increasing or varying streaming speed

I tested the producer with both zero delay and a small artificial delay using EMIT_DELAY_MS. Both runs still produced the requested bounded batch successfully.
Faster input mainly reduces time-to-first-result if the producer and app run concurrently.
Slower input stretches wall-clock runtime and can make the app wait longer in its Kafka polling loop.
In this implementation, the app stops polling after KAFKA_IDLE_TIMEOUT_MS if new events do not arrive, so if the source is too slow and the timeout is too short, the app can finish early or even report that no valid events were consumed.
Throughput-wise, faster streaming increases burst pressure on Kafka consumption and Cassandra writes, but the final analytics content is unchanged if the same event set is processed.


(ii) Changing window function parameters

The current implementation uses a 15-minute sliding event-time window with a 1-minute slide, defined in streamanalyticsapp.py.
Larger windows or smaller slide intervals create more overlap, so each event contributes to more output records.
That increases:
number of analytics rows,
Cassandra write volume,
CPU work for aggregation,
and overall job runtime.
Smaller windows or larger slide intervals reduce output amplification and lower the processing cost, but the analytics become less detailed.
The app also uses OUT_OF_ORDER_MINUTES as watermark tolerance. Increasing it makes the app more tolerant to late data, but it also delays watermark progress and therefore delays when windows are considered complete.
Practical takeaway

Faster producer speed mostly affects latency and burstiness.
Window size and slide affect output amplification and processing cost much more directly.
The combination of a 15-minute window, 1-minute slide, and a real Kafka source explains why a 400-event run produced thousands of Cassandra rows.s

### 4.

Erroneous Data Handling

(i) How I emulate erroneous data in tests

I use the real CSV source file that already contains malformed rows, especially rows with missing humidity values.
The producer validates each CSV row before publishing it, so those malformed rows are naturally exercised during test runs.
The relevant check is in kafka_csv_producer.py:32, and malformed rows are skipped with a warning at kafka_csv_producer.py:88.
On the consumer side, the Flink app also validates Kafka payloads after JSON deserialization, so I can emulate bad streaming data by sending malformed JSON records or records with missing fields; those are rejected in streamanalyticsapp.py:269 and streamanalyticsapp.py:291.
(ii) Test design

The tests are single-tenant and bounded so they finish deterministically.
I run the producer with a capped number of events, then run the Flink app with a bounded Kafka read using MAX_EVENTS and KAFKA_IDLE_TIMEOUT_MS.
The Flink job uses the tenant2 Kafka topic and writes to the tenant2 Cassandra keyspace, so the error-handling behavior is tested in the same configuration as the actual pipeline.
The run configuration is documented in how_to_run_streamanalyticsapp.txt:24 and how_to_run_streamanalyticsapp.txt:44.
In practice, this means I test three things:
malformed source rows are skipped by the producer,
malformed Kafka records are skipped by the Flink consumer,
valid records still flow through to analytics and Cassandra.
(iii) How the implementation deals with erroneous records

In the producer, each CSV row is checked for required fields and type-converted before publishing. Missing or empty fields raise a ValueError, which is caught and logged, then the row is skipped.
In the Flink app, each Kafka message is decoded from JSON and validated again. If a required field is missing or empty, it raises ValueError, logs a warning, and skips the record.
If no valid records are available at all, the app fails fast with no valid events consumed from kafka at streamanalyticsapp.py:340.
Cassandra write failures do not crash the whole job; they are caught and logged at streamanalyticsapp.py:248.
So the behavior is:
malformed input records are skipped,
valid records continue processing,
downstream write errors are contained and logged.
Performance impact

The main cost is extra validation and logging per record, plus exception handling for malformed records.
In this implementation, that cost is acceptable because the job is intentionally single-tenant and bounded, and the error rate in the test data is low.
If bad records were frequent, throughput would drop because the app would spend more time parsing, skipping, and logging, but it would still avoid a full pipeline failure.

### 5.


## Part 3
### 1.

### 2.

### 3.

### 4.

### 5.