FROM golang:1.25-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.Version=${VERSION} -X main.GitCommit=${COMMIT}" -o /server ./cmd/server

FROM gcr.io/distroless/static-debian12

COPY --from=builder /server /server
COPY --from=builder /app/public/ /public/

EXPOSE 9000

ENTRYPOINT ["/server"]
