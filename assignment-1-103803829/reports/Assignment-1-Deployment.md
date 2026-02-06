# This is a deployment/installation guide


0. Initial package downloads:

```
go mod init dataingest
go get github.com/gocql/gocql
go get github.com/segmentio/kafka-go
```
1. Compute batches for the number of workers

```
# get in the right location
# cd assignment-1-103803829/assignment-1-103803829/code/auxx

# python3 chunk_csv.py [file] [workers]
python3 chunk_csv.py ../../data/2025-01-01_bme280_sensor_113.csv 5
```

2. Building cluster:

```
docker compose down -v
docker compose up --build
```

3. Enter Cassandra node and declare namespace

```
docker exec -it cassandra1 cqlsh

# CHECK The cluster is ready (should see DC1 and DC2)
> SELECT data_center, host_id FROM system.peers;

# Run init.cql file
> SOURCE '/init.cql';
```


4. Create Kafka topic with appropriate partition number

```
docker compose exec kafka kafka-topics --bootstrap-server localhost:9092 \
  --create --topic bme280-measurements --partitions 5 --replication-factor 1

docker compose exec kafka kafka-topics --bootstrap-server localhost:9092 --describe --topic bme280-measurements

```

5. Run Ingestion

```
# docker compose --profile workers up -d --build 
#(this will start all available workers declared in docker-compose, which is 12, you usually DON'T want it)

docker compose --profile workers up -d producer0 producer1 producer2 producer3 producer4 consumer0 consumer1 consumer2 consumer3 consumer4


```

6. Monitor logs

```
docker compose logs -f producer0 producer1 producer2 producer3 producer4 producer5 producer6 producer7 producer8 producer9 producer10 producer11
docker compose logs -f consumer0 consumer1 consumer2 consumer3 consumer4 consumer5 consumer6 consumer7 consumer8 consumer9 consumer10 consumer11
```

7. Querying for sanity checks
```
docker exec -it cassandra1 cqlsh
> SELECT * FROM mysimbdp_weather.sensor_measurements_bme280_2025_01_01 LIMIT 5;
> SELECT COUNT(*) FROM mysimbdp_weather.sensor_measurements_bme280_2025_01_01; # returns 550
# Query by hour (efficient - single partition scan):
> SELECT * FROM mysimbdp_weather.sensor_measurements_bme280_2025_01_01 WHERE hour = 10 LIMIT 5;
> SELECT COUNT(*) FROM mysimbdp_weather.sensor_measurements_bme280_2025_01_01 WHERE hour = 10; # returns 24

# Query by hour and sensor (also efficient - filtered within partition):
> SELECT * FROM mysimbdp_weather.sensor_measurements_bme280_2025_01_01 WHERE hour = 10 AND sensor_id = 113 LIMIT 5;

# You can try entering a different cassandra node as well, the same results will be provided
```

8. Tenant Querying script testing

```
./query_counts --day "2025_01_01" --workers [num_workers] 

#Optionally, you can further filter data by

./query_counts --workers [num_workers] --day [day] --hour [hour]
```