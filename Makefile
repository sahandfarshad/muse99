APP=muse99
GO?=go

build:
	$(GO) vet ./...
	$(GO) test ./...
	$(GO) build -o $(APP) .

install: build
	install -m 755 $(APP) $(HOME)/.local/bin/$(APP)

run:
	./$(APP) --allow-shell

web:
	./$(APP) --web --port 3759 --allow-shell

task:
	./$(APP) --task "$(TASK)" --allow-shell

docker:
	docker build -t muse99 .

clean:
	rm -f $(APP) muse99-linux-amd64 muse99-linux-arm64

test:
	$(GO) test -v ./...
