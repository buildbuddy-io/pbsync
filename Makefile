sgp: *.go go.mod go.sum
	go build

go.sum: go.mod
	go mod tidy
	touch go.sum

.PHONY: clean
clean:
	rm -rf sgp
