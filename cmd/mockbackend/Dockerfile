# syntax=docker/dockerfile:1

# --- Build stage: compile a static binary -----------------------------
FROM golang:1.25 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
	-o /out/mockbackend ./cmd/mockbackend

# --- Final stage: minimal runtime image --------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS final
WORKDIR /app

COPY --from=build /out/mockbackend ./mockbackend

# PORT and BACKEND_NAME are read at startup (the -port/-name flags default
# to these env vars), so the same image can run as many differently-named,
# differently-ported instances just by varying environment variables per
# container - no rebuild needed. EXPOSE below documents the image's own
# default; override the published port per container via compose/-p.
ENV PORT=9001
ENV BACKEND_NAME=mockbackend

EXPOSE 9001

ENTRYPOINT ["./mockbackend"]
