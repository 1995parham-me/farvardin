default:
    @just --list

# build the farvardin binary
build:
    @echo '{{ BOLD + CYAN }}Building farvardin{{ NORMAL }}'
    go build -o farvardin ./cmd/farvardin

# run against a local store in ./data
dev: build
    ./farvardin serve --root ./data --listen 127.0.0.1:8000

# update go packages
update:
    go get -u ./...
    go mod tidy

# run tests
test:
    go test -race -v ./... -covermode=atomic -coverprofile=coverage.out

# run golangci-lint
lint:
    golangci-lint run -c .golangci.yml

# lint every card in a directory
lint-cards dir:
    go run ./cmd/farvardin lint -w {{ dir }}

tidy:
    go mod tidy
