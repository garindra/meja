GOOS=darwin GOARCH=arm64 go build -buildvcs=false -o bin/tali-darwin-arm64 ./cmd/tali

go build -buildvcs=false -o bin/tali-server ./cmd/tali-server