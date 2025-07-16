# Build targets
.PHONY: build build-hc build-lb clean dev-setup docker-build docker-build-hc docker-build-lb docker-clean help k8s-delete k8s-deploy k8s-deploy-ns k8s-deploy-redis k8s-deploy-hc k8s-deploy-lb run run-hc run-lb stop stop-hc stop-lb test test-v

# Build both services
build: build-lb build-hc

# Build health checker
build-hc:
	@echo "Building Health Checker..."
	go build -o bin/aetherlay-hc ./services/health-checker/main.go

# Build load balancer
build-lb:
	@echo "Building RPC Load Balancer..."
	go build -o bin/aetherlay-lb ./services/load-balancer/main.go

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	rm -rf bin/
	go clean

# Build Docker images
docker-build: docker-build-hc docker-build-lb

# Build health checker Docker image
docker-build-hc: build-hc
	@echo "Building Health Checker Docker image..."
	docker build -t aetherlay-hc:latest -f services/health-checker/Dockerfile .

# Build load balancer Docker image
docker-build-lb: build-lb
	@echo "Building RPC Load Balancer Docker image..."
	docker build -t aetherlay-lb:latest -f services/load-balancer/Dockerfile .

# Clean Docker images
docker-clean:
	@echo "Cleaning up Docker images..."
	-docker rmi aetherlay-hc:latest aetherlay-lb:latest 2>/dev/null || true

# Development helpers
dev-setup:
	@echo "Setting up development environment..."
	mkdir -p bin
	go mod download

# Kubernetes deployment helpers
k8s-deploy: k8s-deploy-ns k8s-deploy-redis k8s-deploy-hc k8s-deploy-lb

k8s-deploy-ns:
	@echo "Deploying namespace to Kubernetes..."
	kubectl apply -f k8s/namespace.yaml

k8s-deploy-redis:
	@echo "Deploying Redis to Kubernetes..."
	kubectl apply -f k8s/redis.yaml

k8s-deploy-hc:
	@echo "Deploying Health Checker to Kubernetes..."
	kubectl apply -f k8s/health-checker.yaml

k8s-deploy-lb:
	@echo "Deploying Load Balancer to Kubernetes..."
	kubectl apply -f k8s/load-balancer.yaml

k8s-delete:
	@echo "Deleting Kubernetes deployments..."
	kubectl delete -f k8s/load-balancer.yaml
	kubectl delete -f k8s/health-checker.yaml
	kubectl delete -f k8s/namespace.yaml

# Run both services in the background
run: build
	@echo "Starting both services..."
	./bin/aetherlay-hc &
	./bin/aetherlay-lb &
	@echo "Both services started in the background. Use 'ps' to see them."

# Run health checker
run-hc: build-hc
	@echo "Running Health Checker..."
	./bin/aetherlay-hc

# Run load balancer
run-lb: build-lb
	@echo "Running RPC Load Balancer..."
	./bin/aetherlay-lb

# Stop services
stop:
	@echo "Stopping aetherlay-hc and aetherlay-lb..."
	-@pkill -f ./bin/aetherlay-hc || echo "aetherlay-hc not running"
	-@pkill -f ./bin/aetherlay-lb || echo "aetherlay-lb not running"
	@echo "Services stopped."

# Stop health checker
stop-hc:
	@echo "Stopping aetherlay-hc..."
	-@pkill -f ./bin/aetherlay-hc || echo "aetherlay-hc not running"
	@echo "Services aetherlay-hc stopped."

# Stop load balancer
stop-lb:
	@echo "Stopping aetherlay-lb..."
	-@pkill -f ./bin/aetherlay-lb || echo "aetherlay-lb not running"
	@echo "Services aetherlay-lb stopped."

# Test targets
test:
	@echo "Running tests..."
	go test ./...

test-v:
	@echo "Running tests with verbose output..."
	go test -v ./...

# Help
help:
	@echo "Available targets:"
	@echo "  build            - Build both services"
	@echo "  build-hc         - Build health checker only"
	@echo "  build-lb         - Build load balancer only"
	@echo "  clean            - Clean build artifacts"
	@echo "  dev-setup        - Set up development environment"
	@echo "  docker-build     - Build Docker images for both services"
	@echo "  docker-build-hc  - Build Docker image for health checker"
	@echo "  docker-build-lb  - Build Docker image for load balancer"
	@echo "  docker-clean     - Clean Docker images"
	@echo "  k8s-delete       - Delete Kubernetes deployments"
	@echo "  k8s-deploy       - Deploy both services to Kubernetes"
	@echo "  k8s-deploy-hc    - Deploy health checker to Kubernetes"
	@echo "  k8s-deploy-lb    - Deploy load balancer to Kubernetes"
	@echo "  run              - Run both services in the background"
	@echo "  run-hc           - Run health checker"
	@echo "  run-lb           - Run load balancer"
	@echo "  stop             - Stop both services"
	@echo "  stop-hc          - Stop health checker"
	@echo "  stop-lb          - Stop load balancer"
	@echo "  test             - Run tests"
	@echo "  test-v           - Run tests with verbose output"
