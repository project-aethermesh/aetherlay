# Build both services
.PHONY: build
build: build-lb build-hc

# Build health checker
.PHONY: build-hc
build-hc:
	@echo "Building Health Checker..."
	mkdir -p bin
	GOOS=linux GOARCH=amd64 go build -o bin/aetherlay-hc ./services/health-checker/main.go

# Build load balancer
.PHONY: build-lb
build-lb:
	@echo "Building RPC Load Balancer..."
	mkdir -p bin
	GOOS=linux GOARCH=amd64 go build -o bin/aetherlay-lb ./services/load-balancer/main.go

# Clean build artifacts
.PHONY: clean
clean:
	@echo "Cleaning build artifacts..."
	rm -rf bin/
	go clean

# Build Docker images
.PHONY: docker-build
docker-build: docker-build-hc docker-build-lb

# Build health checker Docker image
.PHONY: docker-build-hc
docker-build-hc: build-hc
	@echo "Building Health Checker Docker image..."
	docker build -t aetherlay-hc:latest -f services/health-checker/Dockerfile .

# Build load balancer Docker image
.PHONY: docker-build-lb
docker-build-lb: build-lb
	@echo "Building RPC Load Balancer Docker image..."
	docker build -t aetherlay-lb:latest -f services/load-balancer/Dockerfile .

# Clean Docker images
.PHONY: docker-clean
docker-clean:
	@echo "Cleaning up Docker images..."
	-docker rmi aetherlay-hc:latest aetherlay-lb:latest 2>/dev/null || true

# Run Docker compose
.PHONY: docker-run
docker-run:
	@echo "Running all containers with Docker compose..."
	docker compose -f docker/docker-compose.yml up --build --remove-orphans -d

# Stop Docker compose
.PHONY: docker-stop
docker-stop:
	@echo "Stopping all containers that were started with Docker compose..."
	docker compose -f docker/docker-compose.yml down

# Run the app using preprod images from GHCR
.PHONY: docker-preprod-run
docker-preprod-run:
	@echo "Running the app using preprod images from GHCR..."
	docker compose -f docker/docker-compose.preprod.yml pull
	docker compose -f docker/docker-compose.preprod.yml up --remove-orphans -d

# Stop the app that was started using preprod images from GHCR
.PHONY: docker-preprod-stop
docker-preprod-stop:
	@echo "Stopping the app that was started using preprod images from GHCR..."
	docker compose -f docker/docker-compose.preprod.yml down

# Development helpers
.PHONY: dev-setup
dev-setup:
	@echo "Setting up development environment..."
	mkdir -p bin
	go mod download

# Kubernetes deployment helpers
.PHONY: k8s-deploy
k8s-deploy: k8s-deploy-ns k8s-deploy-redis k8s-deploy-hc k8s-deploy-lb

.PHONY: k8s-deploy-ns
k8s-deploy-ns:
	@echo "Deploying namespace to Kubernetes..."
	kubectl apply -f k8s/namespace.yaml

.PHONY: k8s-deploy-redis
k8s-deploy-redis:
	@echo "Deploying Redis to Kubernetes..."
	kubectl apply -f k8s/redis.yaml

.PHONY: k8s-deploy-hc
k8s-deploy-hc:
	@echo "Deploying Health Checker to Kubernetes..."
	kubectl apply -f k8s/health-checker.yaml

.PHONY: k8s-deploy-lb
k8s-deploy-lb:
	@echo "Deploying Load Balancer to Kubernetes..."
	kubectl apply -f k8s/load-balancer.yaml

.PHONY: k8s-delete
k8s-delete:
	@echo "Deleting Kubernetes deployments..."
	kubectl delete -f k8s/load-balancer.yaml
	kubectl delete -f k8s/health-checker.yaml
	kubectl delete -f k8s/namespace.yaml

# Run both services in the background
.PHONY: run
run: build
	@echo "Starting both services..."
	./bin/aetherlay-hc &
	./bin/aetherlay-lb --metrics-port=9091 &
	@echo "Both services started in the background. Use 'ps' to see them."

# Run health checker
.PHONY: run-hc
run-hc: build-hc
	@echo "Running Health Checker..."
	./bin/aetherlay-hc

# Run load balancer
.PHONY: run-lb
run-lb: build-lb
	@echo "Running RPC Load Balancer..."
	./bin/aetherlay-lb --metrics-port=9091

# Stop services
.PHONY: stop
stop:
	@echo "Stopping aetherlay-hc and aetherlay-lb..."
	-@pkill -f ./bin/aetherlay-hc || echo "aetherlay-hc not running"
	-@pkill -f ./bin/aetherlay-lb || echo "aetherlay-lb not running"
	@echo "Services stopped."

# Stop health checker
.PHONY: stop-hc
stop-hc:
	@echo "Stopping aetherlay-hc..."
	-@pkill -f ./bin/aetherlay-hc || echo "aetherlay-hc not running"
	@echo "Services aetherlay-hc stopped."

# Stop load balancer
.PHONY: stop-lb
stop-lb:
	@echo "Stopping aetherlay-lb..."
	-@pkill -f ./bin/aetherlay-lb || echo "aetherlay-lb not running"
	@echo "Services aetherlay-lb stopped."

# Test targets
.PHONY: test
test:
	@echo "Running tests..."
	go test ./...

.PHONY: test-v
test-v:
	@echo "Running tests with verbose output..."
	go test -v ./...

# Help
.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build               - Build both services"
	@echo "  build-hc            - Build health checker only"
	@echo "  build-lb            - Build load balancer only"
	@echo "  clean               - Clean build artifacts"
	@echo "  dev-setup           - Set up development environment"
	@echo "  docker-build        - Build Docker images for both services"
	@echo "  docker-build-hc     - Build Docker image for health checker"
	@echo "  docker-build-lb     - Build Docker image for load balancer"
	@echo "  docker-clean        - Clean Docker images"
	@echo "  docker-run          - Run local compose (builds images from local source code)"
	@echo "  docker-stop         - Stop local compose"
	@echo "  docker-preprod-run  - Test the preprod images from GHCR before marking them as prod-ready"
	@echo "  docker-preprod-stop - Stop the app that was started using preprod images from GHCR"
	@echo "  k8s-delete          - Delete Kubernetes deployments"
	@echo "  k8s-deploy          - Deploy both services to Kubernetes"
	@echo "  k8s-deploy-hc       - Deploy health checker to Kubernetes"
	@echo "  k8s-deploy-lb       - Deploy load balancer to Kubernetes"
	@echo "  run                 - Run both services in the background"
	@echo "  run-hc              - Run health checker"
	@echo "  run-lb              - Run load balancer"
	@echo "  stop                - Stop both services"
	@echo "  stop-hc             - Stop health checker"
	@echo "  stop-lb             - Stop load balancer"
	@echo "  test                - Run tests"
	@echo "  test-v              - Run tests with verbose output"
