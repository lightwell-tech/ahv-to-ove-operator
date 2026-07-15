FROM golang:1.23 AS builder
WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY main.go main.go
COPY deltasync.go deltasync.go
COPY api/ api/
COPY controllers/ controllers/

RUN CGO_ENABLED=0 GOOS=linux go build -a -o manager main.go deltasync.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
