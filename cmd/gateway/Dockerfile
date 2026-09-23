# syntax=docker/dockerfile:1

# --- Build stage: compile a static binary -----------------------------
FROM golang:1.25 AS build
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
	-o /out/gateway ./cmd/gateway

# --- Final stage: minimal runtime image --------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS final
WORKDIR /app

COPY --from=build /out/gateway ./gateway
COPY config.yaml ./config.yaml

EXPOSE 8080

ENTRYPOINT ["./gateway"]
CMD ["-config", "/app/config.yaml", "-port", "8080"]
