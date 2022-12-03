GIT_HEAD = $(shell git rev-parse HEAD | head -c8)

build:
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(pwd)" -o build/kuber_linux_amd64 -v kuber.go
	GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(pwd)" -o build/kuber_linux_arm64 -v kuber.go

debug:
	go build -ldflags="-X github.com/kubectyl/kuber/system.Version=$(GIT_HEAD)"
	sudo ./kuber --debug --ignore-certificate-errors --config config.yml --pprof --pprof-block-rate 1

build-amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(pwd)" -o build/kuber_linux_amd64 -v kuber.go

build-aarch64:
	GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(pwd)" -o build/kuber_linux_arm64 -v kuber.go

build-docker:
	docker build -t my-image:latest -f Dockerfile.amd64 .
	docker build -t my-image:latest -f Dockerfile.aarch64 .
	manifest-tool push from-args \
	--platforms linux/amd64,linux/arm64 \
	--template my-image:ARCH \
	--target my-image:latest
	docker login -u $(DOCKER_USERNAME) -p $(DOCKER_PASSWORD) docker.pkg.github.com
	docker push docker.pkg.github.com/OWNER/REPO/my-image:latest



# Runs a remotly debuggable session for Kuber allowing an IDE to connect and target
# different breakpoints.
rmdebug:
	go build -gcflags "all=-N -l" -ldflags="-X github.com/kubectyl/kuber/system.Version=$(GIT_HEAD)" -race
	sudo dlv --listen=:2345 --headless=true --api-version=2 --accept-multiclient exec ./kuber -- --debug --ignore-certificate-errors --config config.yml

cross-build: clean build compress

clean:
	rm -rf build/kuber_*

.PHONY: all build build-amd64 build-aarch64 build-docker compress clean