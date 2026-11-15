# Go-only container image. The SvelteKit console is a separate frontend.
FROM golang:1.23-alpine AS build
WORKDIR /work
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/regate ./cmd/regate

FROM alpine:3.20
RUN adduser -D -u 10001 regate && mkdir -p /data /app && chown -R regate:regate /data /app
WORKDIR /app
COPY --from=build /out/regate /app/regate
COPY examples/regate.hcl /app/regate.hcl
COPY skills /app/skills
USER regate
ENV REGATE_SKILLS_DIR=/app/skills
EXPOSE 8080
ENTRYPOINT ["/app/regate", "-config", "/app/regate.hcl", "-state", "/data", "-listen", "0.0.0.0:8080"]
