# Assignment 2 code

This implementation supports local multi-tenant ingestion with:

- One Kafka broker per tenant
- One `streamingestworker` service per tenant (scalable replicas)
- One control component: `mysimbdp-streamingestmanager`
- Shared Cassandra cluster, isolated tenant keyspaces

The active standalone deployment for assignment 2 is:

- `docker-compose.yml` (shared Cassandra cluster)
- `docker-compose.multitenant-brokers.yml` (tenant Kafka/ZooKeeper, sources, workers)

## Is separate Kafka per tenant possible?

Yes. This repo includes `docker-compose.multitenant-brokers.yml` where each tenant has its own ZooKeeper + Kafka pair:

- `tenant1`: `zookeeper-tenant1`, `kafka-tenant1`
- `tenant2`: `zookeeper-tenant2`, `kafka-tenant2`

Both tenants can ingest independently by running `tenant1-source` and `tenant2-source`, with tenant-specific source files and parsing rules defined in `tenant_configs/*.json`.

In the current setup, each tenant can also use a different CSV structure and source file through `tenant_configs/*.json`.

## mysimbdp-streamingestmanager

`mysimbdp-streamingestmanager` is implemented as a CLI component in `streamingestmanager.go`.

### Responsibilities

For each tenant, it can:

1. Start required infra (`cassandra` + tenant Kafka/ZooKeeper)
2. Ensure tenant keyspace exists
3. Ensure tenant topic exists
4. Start/scale `streamingestworker` instances on-demand
5. Stop worker/source instances and optionally stop tenant broker
6. Show tenant or global status

### Commands

```sh
# build manager
go build -o streamingestmanager streamingestmanager.go

# start tenant workers
./streamingestmanager --command start --tenant tenant1 --workers 2

# start tenant workers + source simulator
./streamingestmanager --command start --tenant tenant2 --workers 3 --with-source

# prepare tenant chunks with num_chunks=workers, then start
./streamingestmanager --command start --tenant tenant2 --workers 3 --with-source --prepare-chunks

# check status (all tenants)
./streamingestmanager --command status

# stop tenant workers/source (keep broker running)
./streamingestmanager --command stop --tenant tenant1

# stop tenant workers/source and tenant broker stack
./streamingestmanager --command stop --tenant tenant2 --stop-broker
```

## Black-box model for streamingestworker

`mysimbdp` imposes an invocation contract. The manager does not inspect worker code; it only starts/stops container instances with agreed runtime parameters.

### Contract (required when `WORKER_MODE=managed`)

The worker must read configuration from environment variables:

- `TENANT_ID`
- `KAFKA_BROKERS`
- `KAFKA_TOPIC`
- `KAFKA_CONSUMER_GROUP`
- `CASSANDRA_KEYSPACE`
- `CASSANDRA_HOSTS`

The current `consumer.go` enforces this in managed mode and fails fast when required variables are missing.

## Tenant configuration folder

Tenant-specific behavior is centralized in:

- `tenant_configs/tenant1.json`
- `tenant_configs/tenant2.json`

Each file defines:

- `csv_format` (parsing rules in producer)
- `table_prefix` + `schema_profile` (table/datatype behavior in consumer)
- `source_csv` + `source_chunk_dir` (source input paths)

Supported `csv_format` values:

- `bme280_full` → `sensor_id;sensor_type;location;lat;lon;timestamp;pressure;altitude;pressure_sealevel;temperature;humidity`
- `dht22_compact` → `sensor_id;sensor_type;location;lat;lon;timestamp;temperature;humidity`

## Tenant-specific tables and datatypes

The worker now supports tenant schema profiles selected by `TENANT_ID`.

- `tenant1` (`bme280_full`) writes `pressure`, `altitude`, `pressure_sealevel`, `temperature`, `humidity`
- `tenant2` (`dht22_compact`) writes only `temperature`, `humidity`
- Table prefixes are tenant-specific via config (default examples: `sensor_measurements` for tenant1, `sensor_observations` for tenant2)
- Primary key: `PRIMARY KEY ((day, hour), sensor_id, timestamp)`

Tables are generated dynamically from `sensor_type` and table names are sanitized before execution.

When starting a tenant, `mysimbdp-streamingestmanager` also bootstraps a keyspace-local registry table:

- `<tenant_keyspace>.tenant_schema_registry(tenant_id, schema_profile, updated_at)`

This makes the active schema profile explicit per tenant keyspace.

### Execution steps

1. Tenant (or platform operator) asks manager to start workers.
2. Manager resolves tenant registry entry (`tenant -> broker, topic, keyspace, services`).
3. Manager ensures broker/topic/keyspace exist.
4. Manager starts/scales worker containers.
5. Worker consumes tenant stream and writes only to tenant keyspace.
6. Manager can stop replicas/broker on-demand for pay-per-use.

## What tenant must do to provide a compatible streamingestworker

To make a tenant-developed worker work with `mysimbdp-streamingestmanager`, tenant must:

1. Package worker as a container runnable without interactive input.
2. Implement configuration only via the contract env vars listed above.
3. Consume from `KAFKA_TOPIC` at `KAFKA_BROKERS` with `KAFKA_CONSUMER_GROUP`.
4. Write to `CASSANDRA_KEYSPACE` on `CASSANDRA_HOSTS`.
5. Exit non-zero on unrecoverable startup/config errors; keep running otherwise.
6. Emit logs to stdout/stderr so manager/operator can observe behavior.

## Local run (2 tenants, separate Kafka brokers)

```sh
# from this code directory
docker compose -f docker-compose.yml -f docker-compose.multitenant-brokers.yml up -d \
  cassandra1 cassandra2 cassandra3 zookeeper-tenant1 kafka-tenant1 zookeeper-tenant2 kafka-tenant2

# optional: create keyspaces manually (manager also auto-creates on start)
docker exec -i cassandra1 cqlsh < init_multitenant.cql

# build manager and start workers/sources
go build -o streamingestmanager streamingestmanager.go
./streamingestmanager --command start --tenant tenant1 --workers 2 --with-source
./streamingestmanager --command start --tenant tenant2 --workers 2 --with-source

# optional: regenerate chunks from source CSVs using workers count
./streamingestmanager --command start --tenant tenant1 --workers 2 --with-source --prepare-chunks
./streamingestmanager --command start --tenant tenant2 --workers 2 --with-source --prepare-chunks

# verify
./streamingestmanager --command status
docker exec -it cassandra1 cqlsh -e "SELECT * FROM mysimbdp_tenant1.sensor_measurements_bme280_bronze LIMIT 5;"
docker exec -it cassandra1 cqlsh -e "SELECT * FROM mysimbdp_tenant2.sensor_observations_dht22_bronze LIMIT 5;"
```
