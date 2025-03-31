# KV-Cache

An in-memory key-value cache service with HTTP endpoints for `get` and `put` operations. Designed for performance and low latency under load, with built-in expiration and memory eviction.

## Features
- **Key-Value Operations**: `PUT` (create/update) and `GET` (retrieve).
- **In-Memory Storage**: Sharded map with a simple LRU-like eviction and expiration.
- **Memory Constraints**: Configurable limit (defaults to 256MB). Evicts oldest or largest items when capacity is reached.
- **Metrics**: Exposes Prometheus metrics at `/metrics` for monitoring usage, hits, misses, and evictions.

## Building & Running
1. **Build the Docker Image:**
   ```bash
   docker build -t kv-cache .
   ```
2. **Run the Docker Container:**
   ```bash
   docker run -p 7171:7171 kv-cache
   ```
   - Optionally set environment variables:
     - `PORT` (service port, defaults to `7171`)
     - `CACHE_MAX_MEMORY` (in bytes, defaults to `268435456` for 256MB)

## Usage
- **POST** `/put`
  ```json
  {
    "key": "string up to 256 chars",
    "value": "string up to 256 chars",
    "ttl": "integer (time-to-live in seconds, optional)"
  }
  ```
- **GET** `/get?key=myKey`

## Design Choices
- **Sharding** for concurrency and reduced lock contention.
- **Priority Queue (Heap)** for expired item eviction.
- **LRU Approximation** via tracking last access time and removing large or old entries first.

## Example
```bash
# Insert a key
curl -X POST -H "Content-Type: application/json" \
     -d '{"key":"test","value":"demo"}' \
     http://localhost:7171/put

# Retrieve the key
curl "http://localhost:7171/get?key=test"
```

## License
This project is for educational purposes, provided without warranty.
