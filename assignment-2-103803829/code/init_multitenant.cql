CREATE KEYSPACE IF NOT EXISTS mysimbdp_tenant1
WITH replication = {
  'class': 'NetworkTopologyStrategy',
  'DC1': 2,
  'DC2': 1
};

CREATE KEYSPACE IF NOT EXISTS mysimbdp_tenant2
WITH replication = {
  'class': 'NetworkTopologyStrategy',
  'DC1': 2,
  'DC2': 1
};
