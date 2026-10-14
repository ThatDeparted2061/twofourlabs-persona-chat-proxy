.PHONY: demo demo-quick test build up down

# Reproduce the README numbers locally (needs Go and Redis): baseline vs tuned,
# 1,000 concurrent sessions, primary provider killed mid-run.
demo:
	SESSIONS=1000 KILL_AFTER=45s bench/run.sh

demo-quick:
	SESSIONS=200 KILL_AFTER=20s bench/run.sh

test:
	go vet ./... && go test -race ./...

build:
	go build -o bin/ ./cmd/proxy ./cmd/fakellm ./cmd/loadgen

# Same benchmark in Docker, with Prometheus and Grafana (http://localhost:3000).
up:
	docker compose up --build

down:
	docker compose down -v
