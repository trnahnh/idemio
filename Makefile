IDEMIO_TEST_DATABASE_URL ?= postgres://idemio:idemio@localhost:5433/idemio
IDEMIO_TEST_POOLED_ADDR ?= localhost:6433
IDEMIO_TEST_KAFKA_BROKERS ?= localhost:19092
IDEMIO_TEST_ARCHIVE_ENDPOINT ?= localhost:9000

export IDEMIO_TEST_DATABASE_URL
export IDEMIO_TEST_POOLED_ADDR
export IDEMIO_TEST_KAFKA_BROKERS
export IDEMIO_TEST_ARCHIVE_ENDPOINT

.PHONY: up down test verify load observe drill fmt vet

up:
	docker compose up -d --wait

down:
	docker compose down -v

test:
	go test ./... -count=1

# Everything CI runs. The kill and latency tests are behind build tags because one kills a
# process and the other takes twenty seconds.
verify: fmt vet test
	go test -tags killtest ./internal/reconcile/ -run TestKillMidCall -count=1
	go test -tags latency ./internal/api/ -count=1

# Open loop at a fixed arrival rate: a closed loop would send fewer requests as the system
# slowed, so its percentiles would flatter exactly the case worth measuring. Override with
# IDEMIO_LOAD_RATE, IDEMIO_LOAD_SECONDS and IDEMIO_LOAD_RESOURCES.
load:
	go test -tags loadtest ./internal/api/ -run TestWritePathUnderConcurrentLoad -count=1 -v -timeout 20m

# The deployed binaries plus Prometheus, Alertmanager and Grafana. Off by default: the test
# suite talks to Postgres, the broker and object storage directly and needs none of it.
# Grafana is on :3000, Prometheus :9091, Alertmanager :9093.
observe:
	docker compose --profile observe up -d --build

# ROADMAP Phase 0 exit criterion 6. Forces an indeterminate key against the deployed binary
# and asserts the alert reached a receiver, not merely that the rule evaluated.
drill: observe
	go test -tags drill ./internal/drill/ -count=1 -v -timeout 10m

fmt:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

vet:
	go vet ./...
