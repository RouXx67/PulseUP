# Pulse Makefile for development

.PHONY: build run dev frontend backend all clean dev-hot lint lint-backend lint-frontend format format-backend format-frontend

# Build everything
all: frontend backend

# Build frontend only
frontend:
	cd frontend-modern && npm run build
	@echo "================================================"
	@echo "Copying frontend to internal/api/ for Go embed"
	@echo "This is REQUIRED - Go cannot embed external paths"
	@echo "================================================"
	rm -rf internal/api/frontend-modern
	mkdir -p internal/api/frontend-modern
	cp -r frontend-modern/dist internal/api/frontend-modern/
	@echo "✓ Frontend copied for embedding"

# Build backend only (includes embedded frontend)
backend:
	go build -o pulse ./cmd/pulse

# Build both and run
build: frontend backend

# Run the built binary
run: build
	./pulse

# Development - rebuild everything and restart service
dev: frontend backend
	sudo systemctl restart pulse-hot-dev

dev-hot:
	./scripts/hot-dev.sh

# Clean build artifacts
clean:
	rm -f pulse
	rm -rf frontend-modern/dist

# Quick rebuild and restart for development
restart: frontend backend
	sudo systemctl restart pulse-hot-dev

# Run linters for both backend and frontend
lint: lint-backend lint-frontend

lint-backend:
	golangci-lint run ./...

lint-frontend:
	cd frontend-modern && npm run lint

# Apply formatters
format: format-backend format-frontend

format-backend:
	gofmt -w cmd internal pkg

format-frontend:
	cd frontend-modern && npm run format
