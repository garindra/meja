mkdir -p bin/linux-amd64 bin/darwin-arm64

GOOS=linux GOARCH=amd64 go build -buildvcs=false -o bin/linux-amd64/meja .
GOOS=darwin GOARCH=arm64 go build -buildvcs=false -o bin/darwin-arm64/meja .
