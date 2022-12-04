############################
# STEP 1 build executable binary
############################
FROM golang:1.19 AS builder
# Install git.
# Git is required for fetching the dependencies.
RUN apk update && apk add --no-cache git
WORKDIR $GOPATH/src/mypackage/myapp/
COPY . .
# Fetch dependencies.
# Using go get.
RUN go get -d -v
# Build the binary.
RUN go build -ldflags="-s -w" -gcflags "all=-trimpath=$(pwd)" -o /go/bin/kuber -v kuber.go
############################
# STEP 2 build a small image
############################
FROM scratch
# Copy our static executable.
COPY --from=builder /go/bin/kuber /go/bin/kuber
# Run the hello binary.
ENTRYPOINT ["/go/bin/kuber"]