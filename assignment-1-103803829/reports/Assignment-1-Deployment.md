# This is a deployment/installation guide


0. Initial package downloads:

```
go mod init dataingest
go get github.com/gocql/gocql
go get github.com/segmentio/kafka-go
```

1. Building cluster:

```
docker compose down -v
docker compose up --build
```

2. Enter Cassandra node and declare namespace

```
docker exec -it cassandra1 cqlsh

# CHECK The cluster is ready
> SELECT data_center, host_id FROM system.peers;
> SOURCE '/init.cql';
```

3. Compute batches for the number of workers

```
# python3 chunk_csv.py [file] [workers]
python3 chunk_csv.py assignment-1-103803829/assignment-1-103803829/data/2025-01-01_bme280_sensor_113.csv 12
```

4. Create Kafka topic with appropriate partition number

```
docker compose exec kafka kafka-topics --bootstrap-server localhost:9092 \
  --create --topic bme280-measurements --partitions 12 --replication-factor 1

docker compose exec kafka kafka-topics --bootstrap-server localhost:9092 --describe --topic bme280-measurements

```

5. Run Ingestion

```
docker compose --profile workers up -d --build

```

6. Monitor logs

```
docker compose logs -f producer0 producer1 producer2 producer3 producer4 producer5 producer6 producer7 producer8 producer9 producer10 producer11
docker compose logs -f consumer0 consumer1 consumer2 consumer3 consumer4 consumer5 consumer6 consumer7 consumer8 consumer9 consumer10 consumer11
```

7. Querying for sanity checks
```
docker exec -it cassandra1 cqlsh
> SELECT * FROM mysimbdp_weather.sensor_measurements_BME280_2025_06_01 LIMIT 5;
> SELECT COUNT(*) FROM mysimbdp_weather.sensor_measurements_BME280_2025_06_01;
# Query by hour (efficient - single partition scan):
> SELECT * FROM mysimbdp_weather.sensor_measurements_BME280_2025_06_01 WHERE hour = 10 LIMIT 5;
> SELECT COUNT(*) FROM mysimbdp_weather.sensor_measurements_BME280_2025_06_01 WHERE hour = 10;

# Query by hour and sensor (also efficient - filtered within partition):
> SELECT * FROM mysimbdp_weather.sensor_measurements_BME280_2025_06_01 WHERE hour = 10 AND sensor_id = 113 LIMIT 5;



```