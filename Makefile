.PHONY: build build-windows lint vet clean

build:
	go build ./cmd/officeagent/

build-windows:
	GOOS=windows GOARCH=amd64 go build -o officeagent.exe ./cmd/officeagent/

lint:
	golangci-lint run ./...

vet:
	go vet ./...

clean:
	rm -f officeagent officeagent.exe
