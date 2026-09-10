BINARY := proxyllm

.PHONY: build test test-race lint fmt vet run clean docker

build:
	go build -trimpath -o $(BINARY) ./cmd/proxyllm

test:
	go test ./...

test-race:
	go test -race ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

vet:
	go vet ./...

run: build
	./$(BINARY)

clean:
	rm -f $(BINARY) $(BINARY).exe

docker:
	docker compose -f docker/compose.yaml up --build