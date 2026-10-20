# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src

# Download dependencies first to reuse cached layers.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/userapi ./cmd/userapi

# ---- runtime ----
# distroless/static: no shell or package manager; runs as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/userapi /userapi
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/userapi"]
