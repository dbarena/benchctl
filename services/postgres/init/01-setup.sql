-- Create the benchmark database.
CREATE DATABASE tpcc
    WITH
    OWNER = bench
    ENCODING = 'UTF8'
    TEMPLATE = template0;
