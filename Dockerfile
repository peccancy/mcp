FROM golang:1.26 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /mcp ./cmd

# Stage 2: Runtime
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /mcp /mcp

# Stamped by build.sh at image build time; reported to clients as the server version.
ARG APP_VERSION=""
ARG GIT_SHA=""
ENV APP_VERSION=${APP_VERSION} \
    GIT_SHA=${GIT_SHA}

USER nonroot
EXPOSE 8098

ENTRYPOINT ["/mcp"]
