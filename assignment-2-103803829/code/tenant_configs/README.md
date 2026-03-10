# Tenant configuration files

Each tenant has one JSON file:

- `tenant1.json`
- `tenant2.json`

Fields:

- `tenant_id`: tenant name used by `TENANT_ID`
- `tier`: tenant service tier (`gold` or `silver`) used by consumer consistency policy
- `schema_profile`: logical schema/profile name for logs and registry
- `table_prefix`: Cassandra table prefix used by the consumer
- `csv_format`: parser mode (`bme280_full` or `dht22_compact`)
- `source_csv`: default CSV path used by producer when `CHUNK_NUM` is not set
- `source_chunk_dir`: chunk directory used by producer when `CHUNK_NUM` is set

Tier-to-consistency mapping in the consumer:

- `gold` -> `QUORUM`
- `silver` -> `ONE`

The services load configs from `TENANT_CONFIG_DIR` (default: `./tenant_configs`).
