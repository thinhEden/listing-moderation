# The two targets below are the contract (BRIEF.md §3.1). Needs only make,
# Docker and curl on the host: Go itself runs inside containers.

.PHONY: run test

PORT ?= 8080
export PORT

# One compose project per port, so `PORT=8081 make run` can run next to the
# default instance without sharing containers, networks or the data volume.
RUN_PROJECT := lm-$(PORT)

## run: build and start Postgres + the service in the foreground on $(PORT).
## Ctrl-C or SIGTERM stops and removes the containers and network. The data
## volume is kept, so restarting does not lose listings.
##
## The compose command runs in the background and the shell `wait`s for it:
## a shell only runs its trap while it is waiting, not while a foreground
## child is running, and the trap is what guarantees cleanup on SIGTERM.
run:
	@cleanup() { docker compose -p $(RUN_PROJECT) down --remove-orphans; }; \
	trap 'cleanup; exit 130' INT; \
	trap 'cleanup; exit 143' TERM HUP; \
	docker compose -p $(RUN_PROJECT) up --build --exit-code-from app & \
	pid=$$!; \
	wait $$pid; rc=$$?; \
	trap - INT TERM HUP; \
	cleanup; \
	exit $$rc

## test: run the Go tests (with the race detector) in containers, against a
## throwaway Postgres. No service needs to be running. Exits with the tests'
## exit code and always removes everything it created: containers, network,
## volumes and the test image.
test:
	@p=lm-test-$$$$; \
	cleanup() { docker compose -p $$p -f docker-compose.test.yml down -v --rmi local --remove-orphans >/dev/null 2>&1; }; \
	trap 'cleanup; exit 130' INT; \
	trap 'cleanup; exit 143' TERM HUP; \
	docker compose -p $$p -f docker-compose.test.yml run -T --rm --build test & \
	pid=$$!; \
	wait $$pid; rc=$$?; \
	trap - INT TERM HUP; \
	cleanup; \
	exit $$rc
