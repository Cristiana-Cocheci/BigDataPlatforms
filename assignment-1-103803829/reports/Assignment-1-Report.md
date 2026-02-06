# This your assignment report

**AI Usage Disclosure**:
>I declare that I have not used AI for writing the assignment report\
>I declare that I have used VSCode Copilot for code generation.



## PART 1 - DESIGN

1. The application domain is weather data analysis. The data comes from [Opendata-Stuttgart](https://github.com/opendata-stuttgart/meta/wiki/EN-APIs) and represents weather sensor data.
> The data from the "public" sensors is exported as CSV once a day (around 8:00 am) and made available at http://archive.sensor.community/.

I assumed that the application will load daily the sensor data from multiple .csv files into the Big Data Platform. This tells us that the application is gonna be heavy writes, high volume and time-series (relying on timestamp).

The data used for all further implementation in this assignment is extracted from day 2025-06-01 from sensor BME280. It has 235,7 MB. A row of it can be seen below. The datatype is simple, containing mostly numbers and a timestamp. 

|sensor_id|sensor_type|location|lat|lon|timestamp|pressure|altitude|pressure_sealevel|temperature|humidity|
|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|--:|:--:|
|62779|BME280|48821|48.796|2.396|2025-06-01T02:00:30|101447.88|none|none|21.41|52.48|

Below is described the variety of the data - multiple sensor types and their numbers. The file used for this assignment contained data from all 4736 BME280 sensors. It is estimated that in total, per day, around 1.3 GB of data need to be ingested at once.

|count|sensor_type|
|:--:|:--:|
|4736| bme280|
|129 |bmp180|
|239 |bmp280|
|4032 |dht22|
|29 |ds18b20|
| 5 |hpm|
| 70 |htu21d|
| 229| laerm|
|   11 |pms1003|
|    10 |pms3003|
|    212 |pms5003|
| 2 |pms6003|
|220 |pms7003|
|   2 |ppd42ns|
|   79 |radiation|
|     2 |scd30|
|  10834 |sds011|
|8 |sen5x|
| 1 |sht10|
|  1 |sht11|
|   1 |sht15|
|  116 |sht30|
|   253 |sht31|
|15 |sht35|
|344| sps30|

The intended usecase would be to get hourly statistics on different kind of sensors types for a single sensor or a subset of sensors that the tenant defines.

2. **mysimpdbp** consists of a Cassandra database and a Kafka ingestion system. The different tenants will share a Cassandra cluster of 3 nodes with Quorum consistency. Inside the database, each tenant will own and have access to **only one namespace**. For optimal parallelization and resource allocation - the CSV file will be split into chunks according to the total number of Kafka producers - each producer will be reading from a different file for preventing I/O concurrency bottleneck. The data will then be added into Cassandra through the consumers. 

Kafka and Cassandra are third parties.

**Why Kafka?** The protocol it uses is optimized for high throughput, low latency, and efficient message "batching", which is ideal for a giant dataset that needs to be delivered at once. It gives a lot of integrated features that makes parallelism possible, so that I am able to use to the maximum capacity my resources (since for the first assignment everything runs on my local computer). The Kafka partitioning feature ties nicely with the Cassandra integrated partitioning, so when a consumer tries to insert a batch I am guaranteed that all datasamples will be in the same partition. (Kafka is partitioned by *sensor_id* and Cassandra partitions data by *hour* and then by *sensor_id*).

**Why Cassandra?** It is designed for append only data - which fits the sensor data, because the sensors will never modify a result from the past. It has great partition features (as mentioned in the previous point), which help with querying such a big amount of data. Also, the consistency is integrated, and you can choose it by seelecting a couple of parameters.

**Why Go?** It has integrated concurrency through the go channels. It is very fast.

![Design Schema](../code/auxx/bdp_full_2.drawio.svg)

3. The Cassandra cluster is shared between tenants. It is configured to have 3 nodes, split between 2 datacenters (DC1 and DC2). This enables a resilience against the single-point-of-failure problem. All nodes are holding the same data - this replication grants the desired redundancy.

4. At Cassandra level, replication is 3 - because of the 3 nodes. At Kafka level, ideally replication would also be 2/3 when the number of Brokers is more than 1. 

5. Ideally, the two datacenters would be in different geographical locations, and the dataingest will be as close as possible to the datacenter with more nodes. The **advantage** of this is that the path from tenant to datacenter has to be done either way, so we move the whole process to DC1, for instance. Then during ingestion, since the majority of nodes are in the same location, there is little network traffic. Of course, **adisadvantage** is that there is traffic from DC1 to DC2, but that would also be inevitable, since the nodes from cassandra communicate with eachother. Also, in case of DC1 failure, the traffic would increase.

## PART 2 - IMPLEMENTATION

1. ![Schema implementation](../code/auxx/bdp.drawio%20(1).svg)
For this assignment's implementation everything is run locally on my computer. I have 12 cores, so that s the maximum number of producers/consumers/partitions i can run at once. The resources are very limited, hence the need for heavy parallelization for scalability.

The datacenters in the figure above are only theoretical.

The following figure shows the database schema used. There is only one table called  *mysimbdp_weather.sensor_measurements_BME280_2025_06_01*. This table holds all the data for sensor BME280 from the respective day (230 MB).

![Table](../code/auxx/table_example.drawio.svg)

2. The partitioning inside data ingestion is made by *sensor_id*. This is done so that the consumers can insert a whole batch at once into Cassandra, without any overhead, since all points will end up in the same Cassandra partition anyway. For example,a bad partition key I have tried was (hour, sensor_id) - this was too agressive, so consumers were stuck trying to fill up a batch of points from the same sensor in the same hour, which severly impacted throughput.

The partitioning inside the database is done by *hour* and then by *sensor_id*. We cannot leave the data unpartitioned, because it is too large, and any query would take a long time to run. Considering the usecase I am handling (which is getting hourly statistics on sensor subsets), a reasonable partitioning method is by the hour of the datapoint (fetched from the timestamp), and then secondly by the sensor_id. There are 24 hour partitions, each of approximately 10 MB of data.

In Kafka the set replication is 1, because there is only 1 Broker. However, data is also monitored for being actually ingested by the parameter *RequiredAcks : -1*, which ensures that the producer waits for acknowledgement before considering a message succesfully sent. Also, the number of *MaxAttemts: 3* makes sure that in case of failure, the data will be ressent a maximum of 3 times.

3. The atomic data element to be stored is a single sensor measurement record. Each record is represented as a JSON object with the following fields:

```json
{
  "sensor_id": 62779,
  "sensor_type": "BME280",
  "location": 48821,
  "lat": 48.796,
  "lon": 2.396,
  "day": "2025-06-01",
  "timestamp": "2025-06-01T02:00:30",
  "pressure": 101447.88,
  "altitude": null,
  "pressure_sealevel": null,
  "temperature": 21.41,
  "humidity": 52.48
}
```

In Cassandra, this record is stored as a single row in the table `mysimbdp_weather.sensor_measurements_BME280_2025_06_01` with the primary key structure: `((hour), sensor_id, timestamp)`. The composite partition key `(hour)` combined with clustering keys `(sensor_id, timestamp)` ensures efficient querying by hour and sensor.

The consistency options for Cassandra are ONE (where only 1 node needs to acknowledge a datapoint), QUORUM (where the majority is asked) and ALL (all nodes need to have received the record properly). I chose QUORUM for the balance between speed / availability and consistency.

4. More logs of the different tests can be found in `[benchmark_results](../code/benchmark_results/)`. The following table has been tested on 3 nodes with QUORUM consistency. The dataset size is 3mil records, or 250MB.

|Nodes|Producer Performance (time (s) / throughput (msg/10s))| Consumer Performance (throughput (msg/s))| Comments|
|:--:|:--:|:--:|:--:|
|1|16.61s /175490.22 msg/s |4738.74 records/s over 10.0s| CRASHED after 20 minutes - internal network failure|
|5|59.36s / 9820.54 msg/s|3306.25 records/s over 10.0s| COMPELTED in <2 min|
|10|31.27 / 9322.22 msg/s|5025.97 records/s over 10.0s| COMPELTED in <1.5 min|
|12|26.19s / 9273.62 msg/s| 4219.90 records/s over 10.0s| COMPELTED in <1.5 min|

**!!!** I have also tested different consistency strategies (more replication with 5 nodes, ALL/ ONE consistency etc.), but the performance was virtually the same in all of them, even if theoretically there should have been differences. The assumption is that because everything is done locally on my machine, the transit and messaging complexity is very low. The logs from those can also be found in `[benchmark_results](../code/benchmark_results/)`


**Producer logs - 12 concurrent producers**
```
...
producer0-1   | 2026/02/04 16:52:15 Performance: Duration=26.13s, Throughput=9294.98 msg/s
producer9-1   | 2026/02/04 16:52:15 Produced 2903 messages (total: 242903, processed lines: 242904)
producer9-1   | 2026/02/04 16:52:15 Production complete! Total messages produced: 242903, Total lines processed: 242904
producer9-1   | 2026/02/04 16:52:15 Performance: Duration=26.26s, Throughput=9249.89 msg/s
producer1-1   | 2026/02/04 16:52:15 Produced 2903 messages (total: 242903, processed lines: 242904)
producer1-1   | 2026/02/04 16:52:15 Production complete! Total messages produced: 242903, Total lines processed: 242904
producer1-1   | 2026/02/04 16:52:15 Performance: Duration=26.15s, Throughput=9288.94 msg/s
producer7-1   | 2026/02/04 16:52:15 Produced 2903 messages (total: 242903, processed lines: 242904)
producer7-1   | 2026/02/04 16:52:15 Production complete! Total messages produced: 242903, Total lines processed: 242904
producer7-1   | 2026/02/04 16:52:15 Performance: Duration=26.19s, Throughput=9273.62 msg/s
producer1-1 exited with code 0
producer5-1 exited with code 0
producer3-1 exited with code 0
producer2-1 exited with code 0
producer4-1 exited with code 0
producer0-1 exited with code 0
producer10-1 exited with code 0
producer9-1 exited with code 0
```

**Consumer logs**
```
2026/02/05 20:46:25 Created table: mysimbdp_weather.sensor_measurements_BME280_2025_06_01

... 

2026/02/05 20:46:35 Throughput: 1618.23 records/s over 10.0s (total inserted: 16200)

... 

2026/02/05 20:47:48 Inserted 25 records (total: 243650, consumed messages: 243650)
```

**Testing after ingestion**
```
 cqlsh> SELECT COUNT(*) FROM mysimbdp_weather.sensor_measurements_BME280_2025_06_01 WHERE hour = 10;


 count
--------
 122388


 cqlsh> SELECT COUNT(*) FROM mysimbdp_weather.sensor_measurements_BME280_2025_06_01 WHERE hour = 0 and sensor_id=113;

 count
-------
    24
```

5. The script to querying data as a tenant can be found in [query_counts.go](../code/query_counts.go). It implements the basic usecase of querying hourly data about sensors. I have tested it for multiple concurrent workers. The logs of the results can be seen in [tenant_tests](../code/benchmark_results/tenant_tests.txt). The query is looking through almost 3 million records and the performance is as follows:

|workers|speed (seconds)|errors|redords queried|
|:--:|:--:|:--:|:--:|
|1| 9.24 | 0|2911563|
|4| 2.89 | 0|2911563|
|8| 2.46 | 0|2911563|
|12| 2.6 | 0|2911563|


```
./query_counts
2026/02/05 22:52:24 Connected to Cassandra. Table: mysimbdp_weather.sensor_measurements_BME280_2025_06_01
2026/02/05 22:52:24 Using 4 parallel workers

Hour     Count           Status      
-----------------------------------
0        121010          OK          
1        120596          OK          
2        121226          OK          
3        121270          OK          
4        121716          OK          
5        122160          OK          
6        121972          OK          
7        122047          OK          
8        122059          OK          
9        121469          OK          
10       122388          OK          
11       122080          OK          
12       122113          OK          
13       122285          OK          
14       122394          OK          
15       122311          OK          
16       122187          OK          
17       120226          OK          
18       112774          OK          
19       121233          OK          
20       121961          OK          
21       121507          OK          
22       121348          OK          
23       121231          OK          
-----------------------------------
Total records: 2911563
Total time: 2.89 seconds
Errors: 0
```



## PART 3 - EXTENSION

1. The types of lineage that would be useful to support are:
 - source lineage : original data source, original file name, size, timestamp
 - ingestion lineage : producer/consumer instance id, partition assignment, ingestion timestamp
 - database lineage : number of replication, nodes, consistency 
 - quality control : number of ingested vs. stored records

They would be captured at the levels of [producer.go](../code/producer.go), at broker level, at [consumer.go](../code/consumer.go)...

They would be stored in a separate lineage table inside Cassandra.

Example: 

```json
{
  "lineage_id": "lineage-2025-06-01-bme280-tenant123",
  "tenant_id": "tenant-123",
  "source": {
    "type": "http-csv",
    "url": "http://archive.sensor.community/2025-06-01_bme280.csv",
    "file_size_bytes": 247123456,
    "record_count": 2914834,
    "download_timestamp": "2025-06-01T08:15:00Z"
  },
  "dataset": {
    "sensor_type": "BME280",
    "day": "2025-06-01",
    "cassandra_table": "mysimbdp_weather.sensor_measurements_BME280_2025_06_01"
  },
  "pipeline": {
    "kafka_topic": "bme280-measurements",
    "producer_id": "producer-3",
    "kafka_partitions_used": 12,
    "consumer_id": "consumer-5",
    "replication_factor": 1
  },
  "execution": {
    "start_time": "2025-06-01T08:16:00Z",
    "end_time": "2025-06-01T08:18:26Z",
    "duration_seconds": 146
  },
  "data_quality": {
    "records_ingested": 2914834,
    "records_stored": 2914834,
    "records_failed": 0,
    "consistency_level": "QUORUM"
  }
}
```

2. 
Since each tenant needs a dedicated mysimbdp-coredms, the registry (etcd/Consul/ZooKeeper) would store a hierarchical mapping of tenant - coredms configuration. The schema would include:

**Registry Structure:**
```
/mysimbdp/
  /tenants/
    /tenant-1/
      /coredms/
        - cassandra_hosts: ["cassandra-tenant1-1:9042", "cassandra-tenant1-2:9042", "cassandra-tenant1-3:9042"]
        - cassandra_keyspace: "mysimbdp_weather_tenant1"
        - replication_factor: 3
        - consistency_level: "QUORUM"
        - datacenter: "DC1"
        - status: "healthy"
      /kafka/
        - brokers: ["kafka-tenant1:9092"]
        - topic_prefix: "tenant1-"
        - partitions: 12
    
    /tenant-2/
      /coredms/
        - cassandra_hosts: ["cassandra-tenant2:9042"]
        - cassandra_keyspace: "mysimbdp_weather_tenant2"
        - replication_factor: 3
        - consistency_level: "QUORUM"
        - datacenter: "DC2"
        - status: "healthy"
      ...
```
<!-- 
**Discovery Workflow:**
1. Tenant "tenant-123" wants to ingest data
2. mysimbdp-dataingest queries registry: `GET /mysimbdp/tenants/tenant-123/coredms`
3. Gets back: cassandra_hosts, keyspace, consistency_level
4. Connects to the dedicated Cassandra cluster for that tenant -->

This schema enables service discovery by storing which Cassandra cluster (mysimbdp-coredms) belongs to which tenant, allowing the ingestion pipeline to dynamically discover and connect to the correct database instance.

3. **Integrating Service and Data Discovery into mysimbdp-dataingest:**

To integrate with the discovery schema defined in Q2, mysimbdp-dataingest (producer.go and consumer.go) would need the following changes:
- Add etcd/Consul client library
- On startup, query `/mysimbdp/tenants/{tenant_id}/kafka/brokers` to get Kafka broker addresses instead of hardcoded `kafka:29092`
- On startup, query `/mysimbdp/tenants/{tenant_id}/coredms` to get Cassandra hosts, keyspace, and consistency_level instead of hardcoded values, then connect to the desired Cassandra cluster


With this integration, multi-tenant instances automatically discover their dedicated mysimbdp-coredms cluster, can scale without code changes, and the system tracks which producer/consumer instances are healthy.

4. With a new Data-as-a-Service (DaaS) layer, I change mysimbdp-dataingest to write through mysimbdp-daas instead of directly to Cassandra. Architecture:

![DAAS](../code/auxx/daas.drawio.svg)

Benefits: Single entry point for all data access, enforces tenant isolation and enforces authentication. External producers/consumers also use mysimbdp-daas APIs to read/write, ensuring consistent access patterns and security.

5. 
For sensor data, a natural constraint would be:
- **Hot space:** Data from the last 30 days. These records have high query frequency (real-time dashboards, recent trend analysis, anomaly detection), require low latency (<100ms), and are frequently updated/verified. This data is stored on fast SSD-based Cassandra nodes with replication factor 3.
- **Cold space:** Data older than 30 days. These records have low query frequency (historical analysis, compliance archival), can tolerate higher latency (1-5s), and are immutable. This data is stored on cheaper nodes or object storage (S3/Azure Blob) with replication factor 1.

For migration from Hot to Cold, a daily service could run, that selects the tables from the days that are old enough and exports them to the cold storage, after which it delets them from the hot one.

A possible inconsistency problem is if a query arrives during migration, it may hit partially-migrated data. This should be avoided by marking data as *in-transit-data*.
