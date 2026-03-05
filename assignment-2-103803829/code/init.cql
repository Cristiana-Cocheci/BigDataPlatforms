DROP KEYSPACE IF EXISTS mysimbdp_weather;

CREATE KEYSPACE IF NOT EXISTS mysimbdp_weather
WITH replication = {
  'class': 'NetworkTopologyStrategy',
  'DC1': 2,
  'DC2': 1
};