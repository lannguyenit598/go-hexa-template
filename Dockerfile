# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src

# Tải dependency trước để tận dụng cache layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/userapi ./cmd/userapi

# ---- runtime ----
# distroless/static: không shell, không package manager, chạy user không phải root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/userapi /userapi
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/userapi"]
