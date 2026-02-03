DROP KEYSPACE IF EXISTS mysimbdp_weather;

CREATE KEYSPACE IF NOT EXISTS mysimbdp_weather
WITH replication = {
  'class': 'NetworkTopologyStrategy',
  'DC1': 2,
  'DC2': 1
};


CREATE TABLE IF NOT EXISTS mysimbdp_weather.sensor_measurements_by_sensor_day (
    sensor_id int,
    sensor_type text,
    location float,
    lat float,
    lon float,
    day text,
    timestamp text,
    pressure float,
    altitude float,
    pressure_sealevel float,
    temperature float,
    humidity float,
    PRIMARY KEY ((sensor_id, day), timestamp)
);
