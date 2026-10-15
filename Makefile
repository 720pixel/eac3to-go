.PHONY: build windows windows-decoder decoder test check clean

build:
	mkdir -p bin
	CGO_ENABLED=1 go build -tags=netgo,osusergo -buildvcs=false -trimpath -ldflags='-s -w -linkmode external -extldflags "-static"' -o bin/eac3to ./cmd/eac3to
	CGO_ENABLED=0 go build -tags=netgo,osusergo -buildvcs=false -trimpath -ldflags='-s -w' -o bin/pcmcompare ./cmd/pcmcompare

decoder:
	python3 scripts/build-decoder.py

windows:
	python3 scripts/build-windows.py

windows-decoder:
	python3 scripts/build-windows-decoder.py $(if $(WINDOWS_RUNNER),--runner "$(WINDOWS_RUNNER)")

test:
	go test ./...

check:
	go vet ./...
	go test -race ./...

clean:
	rm -f bin/eac3to bin/pcmcompare
	rm -f bin/windows-amd64/eac3to.exe bin/windows-amd64/pcmcompare.exe
