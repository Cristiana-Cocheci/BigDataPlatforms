# This your assignment report

**AI Usage Disclosure**:
>I declare that I have not used AI for writing the assignment report
>I declare that I have not used/I have used XYZ for code generation (include scripts, configuration, ...)



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

![Design Schema](../bdp_full_2.drawio.svg)

3. The Cassandra cluster is shared between tenants. It is configured to have 3 nodes, split between 2 datacenters (DC1 and DC2). This enables a resilience against the single-point-of-failure problem. All nodes are holding the same data - this replication grants the desired redundancy.

4. At Cassandra level, replication is 3 - because of the 3 nodes. At Kafka level, ideally replication would also be 2/3 when the number of Brokers is more than 1. 

5. Ideally, the two datacenters would be in different geographical locations, and the dataingest will be as close as possible to the datacenter with more nodes. The advantage of this is that the path from tenant to datacenter has to be done either way, so we move the whole process to DC1, for instance. Then during ingestion, since the majority of nodes are in the same location, there is little network traffic. Of course, there is traffic from DC1 to DC2, but that would also be inevitable, since the nodes from cassandra communicate with eachother.

## PART 2 - IMPLEMENTATION

1. ![Schema implementation](../bdp.drawio%20(1).svg)
For this assignment's implementation everything is run locally on my computer. I have 12 cores, so that s the maximum number of producers/consumers/partitions i can run at once.

The datacenters in the figure above are only theoretical.

The following figure shows the database schema used. There is only one table called  *mysimbdp_weather.sensor_measurements_BME280_2025_06_01*. This table holds all the data for sensor BME280 from the respective day (230 MB).

![Table](../table_example.drawio.svg)

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

4. Logs of the different tests can be found in [benchmark_results](../code/benchmark_results/). The following table has been tested on 3 nodes with QUORUM consistency.

|Nodes|Producer Performance (time (s) / throughput (msg/10s))| Consumer Performance (throughput (msg/s))| Comments|
|:--:|:--:|:--:|:--:|
|1|16.61s /175490.22 msg/s |4738.74 records/s over 10.0s| CRASHED after 20 minutes - internal network failure|
|5|59.36s / 9820.54 msg/s|3306.25 records/s over 10.0s| COMPELTED in <2 min|
|10|31.27 / 9322.22 msg/s|5025.97 records/s over 10.0s| COMPELTED in <1.5 min|
|12|26.19s / 9273.62 msg/s| 4219.90 records/s over 10.0s| COMPELTED in <1.5 min|

I have also tested different consistency strategies (more replication with 5 nodes, ALL/ ONE consistency etc.), but the performance was virtually the same in all of them, even if theoretically there should have been differences. The assumption is that because everything is done locally on my machine, the transit and messaging complexity is very low.

5. The script to querying data as a tenant can be found in [query_counts.go](../code/query_counts.go). It implements the basic usecase of querying hourly data about sensors. I have tested it for multiple concurrent workers. The logs of the results can be seen in [tenant_tests](../code/benchmark_results/tenant_tests.txt). The query is looking through almost 3 million records and the performance is as follows:

|workers|speed (seconds)|errors|
|:--:|:--:|:--:|
|1| 9.24 | 0|
|4| 2.89 | 0|
|8| 2.46 | 0|
|12| 2.6 | 0|





## PART 3 - EXTENSION

1. 

2.

3.

4.

5. 
For sensor data, a natural constraint would be:
- **Hot space:** Data from the last 30 days. These records have high query frequency (real-time dashboards, recent trend analysis, anomaly detection), require low latency (<100ms), and are frequently updated/verified. This data is stored on fast SSD-based Cassandra nodes with replication factor 3.
- **Cold space:** Data older than 30 days. These records have low query frequency (historical analysis, compliance archival), can tolerate higher latency (1-5s), and are immutable. This data is stored on cheaper HDD-based nodes or object storage (S3/Azure Blob) with replication factor 1.

For migration from Hot to Cold, a daily service could run, that selects the tables from the days that are old enough and exports them to the cold storage, after which it delets them from the hot one.

A possible inconsistency problem is if a query arrives during migration, it may hit partially-migrated data. This should be avoided by marking data as *in-transit-data*.
