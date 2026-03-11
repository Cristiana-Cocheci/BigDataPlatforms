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

Silver pipeline runtime config:

- `silverpipeline_<tenant>.yaml` stores pipeline constraints and optional runtime settings for the silver stage.
- `pipeline.storage_backend`: cache backend (`local` or `gcs`). Default is `local`.
- `pipeline.cache_dir`: local cache folder used when `pipeline.storage_backend=local`.
- `pipeline.logging.dir`: base directory for silverpipeline file logs.
- `pipeline.logging.run_file`: run-level JSONL log filename/path (pipeline success/failure, total duration, config context).
- `pipeline.logging.task_file`: task-level JSONL log filename/path (extract/transform/insert task status, duration, row counts, data sizes).
- `pipeline.gcs.bucket`: target bucket used when `pipeline.storage_backend=gcs`.
- `pipeline.gcs.prefix`: object prefix used when `pipeline.storage_backend=gcs`.
- `pipeline.gcs.credentials_file`: optional in-container credentials JSON path for GCS mode.
- `pipeline.extract_page_size`: Cassandra page size used when extracting bronze rows.
- `pipeline.extract_day`: bronze day partition (`YYYY-MM-DD`) extracted by `extract-cache` and `full` modes.
- `pipeline.batchmanager.input_glob`: cache file pattern scanned by `mysimbdp-batchmanager`.
- `pipeline.batchmanager.state_file`: per-tenant batchmanager state file used to avoid reprocessing unchanged cache files.
- `pipeline.transformation.drop_rows_with_missing_entries`: drops rows that contain any blank field before aggregation.
- `pipeline.transformation.metric_fields`: numeric bronze columns aggregated into silver `avg`, `min`, `max`, and `median` columns.
