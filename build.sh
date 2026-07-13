GOOS=darwin GOARCH=arm64 go build -buildvcs=false -o bin/tali-darwin-arm64 ./cmd/tali

go build -buildvcs=false -o bin/tali-ctrl ./cmd/tali-ctrl
