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
./bin/aetherlay-hc -config /custom/path/endpoints.json -redis-addr localhost:6379
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

### Command Line Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `configs/endpoints.json` | Path to endpoints configuration file |
| `-health-check-interval` | `30` | Health check interval in seconds (overrides `HEALTH_CHECK_INTERVAL` env var) |
| `-redis-addr` | `localhost:6379` | Redis server address |
| `-standalone-health-checks` | `true` | Enable standalone health checks (overrides `STANDALONE_HEALTH_CHECKS` env var) |

> **Note:** Command-line flags take precedence over environment variables if both are set.

### Configuration File

The health checker uses the same configuration format as the load balancer:

```json
{
  "mainnet": {
    "infura": {
      "provider": "infura",
      "rpc_url": "https://mainnet.infura.io/v3/YOUR_API_KEY",
      "ws_url": "wss://mainnet.infura.io/ws/v3/YOUR_API_KEY",
      "role": "primary",
      "type": "full",
      "weight": 1
    },
    "alchemy": {
      "provider": "alchemy",
      "rpc_url": "https://eth-mainnet.alchemyapi.io/v2/YOUR_API_KEY",
      "ws_url": "wss://eth-mainnet.alchemyapi.io/v2/YOUR_API_KEY",
      "role": "fallback",
      "type": "full",
      "weight": 2
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

## Redis Data Structure

The health checker stores data in Redis with the following key patterns:

### Health Status
```
health:{chain}:{provider} -> JSON encoded EndpointStatus
```

#### Example EndpointStatus JSON
```json
{
  "health_status": true,
  "last_health_check": "2024-06-10T12:34:56.789Z",
  "requests_24h": 1234,
  "requests_1_month": 5678,
  "requests_lifetime": 9012
}
```

### Request Counts
```
metrics:{chain}:{provider}:proxy_requests:requests_24h -> int64
metrics:{chain}:{provider}:proxy_requests:requests_1m -> int64
metrics:{chain}:{provider}:proxy_requests:requests_all -> int64
metrics:{chain}:{provider}:health_requests:requests_24h -> int64
metrics:{chain}:{provider}:health_requests:requests_1m -> int64
metrics:{chain}:{provider}:health_requests:requests_all -> int64
```

## Monitoring

### Health Checker Status

The health checker logs its activity with structured logging:

```json
{
  "level": "info",
  "chain": "mainnet",
  "provider": "infura",
  "healthy": true,
  "message": "Updated endpoint status"
}
```

### Redis Monitoring

Monitor Redis keys to track health status:

```bash
# Check health status for all endpoints
redis-cli keys "health:*"

# Get specific endpoint status
redis-cli get "health:mainnet:infura"

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
   - Check REDIS_HOST and REDIS_PORT environment variables

2. **Configuration File Not Found**
   ```
   Failed to load configuration: open configs/endpoints.json: no such file or directory
   ```
   - Ensure configuration file exists
   - Check file permissions

3. **Health Check Timeouts**
   ```
   Health check request failed: context deadline exceeded
   ```
   - Increase HTTP client timeout
   - Check network connectivity to RPC endpoints

### Debug Mode

Enable verbose logging by setting the log level:

```bash
export ZEROLOG_LEVEL=debug
./bin/aetherlay-hc
```

## Integration with Load Balancer

### Load Balancer Configuration

To use the standalone health checker, configure your load balancer pods with:

```yaml
env:
- name: STANDALONE_HEALTH_CHECKS
  value: "true"
```

### Verification

1. **Check Health Checker is Running**:
   ```bash
   kubectl logs -f deployment/aetherlay-hc
   ```

2. **Verify Load Balancer Health**:
   ```bash
   kubectl logs deployment/rpc-load-balancer | grep "Health checks disabled"
   ```

3. **Test Endpoint Health**:
   ```bash
   # Check Redis for health status
   redis-cli get "health:mainnet:infura"
   ```
