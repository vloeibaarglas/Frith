# Build stage
FROM golang:1.24-alpine AS build
WORKDIR /app
COPY go.mod ./
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/frith ./cmd/frith

# Runtime: statically-linked binary + empty data dir. ~10 MB total.
FROM scratch
COPY --from=build /out/frith /frith
ENV UPLOAD_ADDR=:8080
ENV UPLOAD_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/frith"]
