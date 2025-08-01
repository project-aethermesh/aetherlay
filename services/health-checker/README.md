# Ætherlay - Standalone Health Checker

This is a standalone health checker service that monitors RPC endpoints and updates their health status in Redis. It's designed to work alongside multiple load balancer instances that have health checks disabled.

## Overview

The standalone health checker provides mainly these 2 benefits:

- **RPC endpoints are used efficiently** by eliminating the chance of having redundant health checks.
- **Load balancer pods can scale independently** of the health checker.

## Quick Start

### 1. Build the Health Checker

```bash
make build-hc
```

### 2. Run it

```bash
make run-hc

# Or with custom config
./bin/aetherlay-hc --config /custom/path/endpoints.json --redis-host localhost --redis-port 6379
```

### 3. Run with Docker

```bash
# Build Docker image
make docker-build-hc

# Run container
docker run -d \
  --name aetherlay-hc \
  -e REDIS_HOST=localhost \
  -e REDIS_PORT=6379 \
  -e HEALTH_CHECK_INTERVAL=60 \
  -v $(pwd)/configs:/root/configs \
  aetherlay-hc:latest
```

Make sure to add all the env vars you need, based on your specific configs.

### 4. Deploy to Kubernetes

```bash
make k8s-deploy
```

If you only want to deploy the health checker:

```bash
# Create namespace
make k8s-deploy-ns

# Deploy Redis if needed
make k8s-deploy-redis

# Deploy health checker
make k8s-deploy-hc
```

### Configuration File

The health checker uses the same configuration format as the load balancer:

```json
{
  "mainnet": {
    "infura": {
      "provider": "infura",
      "role": "primary",
      "type": "full",
      "http_url": "https://mainnet.infura.io/v3/YOUR_API_KEY",
      "ws_url": "wss://mainnet.infura.io/ws/v3/YOUR_API_KEY"
    },
    "alchemy": {
      "provider": "alchemy",
      "role": "fallback",
      "type": "full",
      "http_url": "https://eth-mainnet.alchemyapi.io/v2/YOUR_API_KEY",
      "ws_url": "wss://eth-mainnet.alchemyapi.io/v2/YOUR_API_KEY"
    }
  }
}
```

## Health Check Process

1. **Endpoint Discovery**: Reads all endpoints from configuration.
2. **Concurrent Checking**: Checks multiple endpoints simultaneously using goroutines.
3. **JSON-RPC Validation**: Sends `eth_blockNumber` requests to validate endpoint health.
4. **Status Update**: Updates Redis with health status and request counts.
5. **Logging**: Provides detailed logs for monitoring and debugging.

### Note on Ephemeral Health Checks

- The environment variable `EPHEMERAL_CHECKS_INTERVAL` is only relevant for the load balancer when regular health checks are disabled (`HEALTH_CHECK_INTERVAL=0`).
- In this mode, the load balancer will use ephemeral health checks to monitor failed endpoints at the specified interval (default: 30s).
- The standalone health checker will not run if `HEALTH_CHECK_INTERVAL=0`.

## Redis Data Structure

The health checker stores data in Redis with the following key patterns:

### Health Status
```
health:{chain}:{endpoint} -> JSON encoded EndpointStatus
```

#### Example EndpointStatus JSON
```json
{
  "last_health_check": "2024-06-10T12:34:56.789Z",
  "requests_24h": 1234,
  "requests_1_month": 5678,
  "requests_lifetime": 9012,
  "has_http": true,
  "has_ws": true,
  "healthy_http": true,
  "healthy_ws": false
}
```

### Request Counts
```
metrics:{chain}:{endpoint_id}:proxy_requests:requests_24h -> int64
metrics:{chain}:{endpoint_id}:proxy_requests:requests_1m -> int64
metrics:{chain}:{endpoint_id}:proxy_requests:requests_all -> int64
metrics:{chain}:{endpoint_id}:health_requests:requests_24h -> int64
metrics:{chain}:{endpoint_id}:health_requests:requests_1m -> int64
metrics:{chain}:{endpoint_id}:health_requests:requests_all -> int64
```

**Note**: The 24-hour and 1-month counters have automatic expiration set (24 hours and 30 days respectively). The lifetime counter persists indefinitely.

## Monitoring

You can use Redis keys to track health status:

```bash
# Check health status for all endpoints
redis-cli keys "health:*"

# Get specific endpoint status
redis-cli get "health:mainnet:infura-1"

# Monitor request counts
redis-cli keys "metrics:*"
```

## Troubleshooting

### Common Issues

1. **Redis Connection Failed**
   ```
   Failed to connect to Redis: dial tcp: connection refused
   ```
   - Verify Redis is running
   - Check REDIS_HOST, REDIS_PORT and REDIS_PASS environment variables

2. **Configuration File Not Found**
   ```
   Failed to load configuration: open configs/endpoints.json: no such file or directory
   ```
   - Ensure configuration file exists
   - Check file permissions

### Debug Mode

Enable verbose logging by setting the log level:

```bash
export LOG_LEVEL=debug
./bin/aetherlay-hc
```

You can also use the CLI flag (takes precedence over the environment variable):

```bash
./bin/aetherlay-hc --log-level=debug
```
