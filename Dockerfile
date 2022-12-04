FROM golang:1.19 as build

WORKDIR /go/src/app
COPY . .

RUN go mod download
RUN go vet -v
RUN go test -v

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(pwd)" -o /go/bin/kuber -v kuber.go

FROM gcr.io/distroless/static-debian11

COPY --from=build /go/bin/kuber /
CMD ["/kuber"]