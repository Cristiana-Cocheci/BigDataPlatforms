CREATE KEYSPACE IF NOT EXISTS mysimbdp_tenant2
WITH replication = {
  'class': 'SimpleStrategy',
  'replication_factor': 1
};
