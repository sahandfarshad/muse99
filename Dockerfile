# Muse99 — production image
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum* ./
COPY *.go ./
RUN go vet ./... && go test ./... && CGO_ENABLED=0 go build -o muse99 .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git openssh-client curl && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /src/muse99 /usr/local/bin/muse99
COPY README.md .
EXPOSE 8080
ENV MODEL_API_BASE_URL=https://api.meta.ai/v1
ENV MODEL_ID=muse-spark-1.3
# Run: docker run -e MODEL_API_KEY=... -p 8080:8080 -v muse99-data:/root/.muse99 -v $PWD/work:/app/work muse99 --menu=false --web --port 8080 --allow-shell
ENTRYPOINT ["muse99"]
