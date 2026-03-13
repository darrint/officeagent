.PHONY: build build-windows dev lint vet clean

build:
	go build ./cmd/officeagent/

build-windows:
	GOOS=windows GOARCH=amd64 go build -o officeagent.exe ./cmd/officeagent/

dev:
	air

lint:
	golangci-lint run ./...

vet:
	go vet ./...

clean:
	rm -f officeagent officeagent.exe
	rm -rf tmp/
