IDEMIO_TEST_DATABASE_URL ?= postgres://idemio:idemio@localhost:5433/idemio
IDEMIO_TEST_POOLED_ADDR ?= localhost:6433
IDEMIO_TEST_KAFKA_BROKERS ?= localhost:19092
IDEMIO_TEST_ARCHIVE_ENDPOINT ?= localhost:9000

export IDEMIO_TEST_DATABASE_URL
export IDEMIO_TEST_POOLED_ADDR
export IDEMIO_TEST_KAFKA_BROKERS
export IDEMIO_TEST_ARCHIVE_ENDPOINT

.PHONY: up down test verify fmt vet

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

fmt:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

vet:
	go vet ./...
