.PHONY: up down logs demo traces assert test check diagram

# The whole stack, from a clean checkout, in one command.
up:
	docker compose up -d --build --wait

down:
	docker compose down -v

logs:
	docker compose logs -f --tail=50

# A short play session: a few matches, a few players, and one payload a client got wrong.
demo:
	@for m in 1 2 3; do \
	  for p in 1 2 3 4; do \
	    curl -fsS -X POST -d "{\"match_id\":\"m-$$m\",\"player_id\":\"p-$$p\",\"action\":\"join\"}" http://localhost:18081/v1/play >/dev/null; \
	  done; \
	done
	@for i in 1 2 3 4 5 6 7 8 9 10; do \
	  curl -fsS -X POST -d '{"match_id":"m-1","player_id":"p-1","action":"move"}' http://localhost:18081/v1/play >/dev/null; \
	done
	@curl -s -o /dev/null -w "a payload with no player: HTTP %{http_code} (counted as invalid, not as a failure)\n" \
	  -X POST -d '{"match_id":"m-1"}' http://localhost:18081/v1/play

# The claim this repository makes, checked against the running stack rather than asserted in a README.
traces:
	@curl -fsS "http://localhost:16686/api/traces?service=gameserver&limit=5&lookback=1h" \
	  | python3 -c "import sys,json; d=json.load(sys.stdin); ts=d['data']; print(len(ts),'traces'); [print(' ', t['traceID'][:16], t['spans'][0]['operationName'], t['spans'][0]['duration'],'us') for t in ts[:5]]"

# The same assertion CI runs, so a local stack can be asked the same question. Ports can be moved with
# JAEGER_URL / PROMETHEUS_URL, and the retry budget with ATTEMPTS / DELAY.
assert:
	python3 scripts/check-telemetry.py

test:
	go test -race -count=1 ./...

check:
	gofmt -l . | tee /dev/stderr | wc -l | grep -qx 0
	go vet ./...
	go test -race -count=1 ./...
	docker compose config -q

# Sources are HTML and Mermaid; a PNG is a build artifact. No browser is needed to read them.
diagram:
	@if command -v chromium >/dev/null 2>&1; then B=chromium; elif command -v google-chrome >/dev/null 2>&1; then B=google-chrome; else echo "no chromium on PATH: open docs/diagrams/*.html in a browser"; exit 0; fi; \
	for f in docs/diagrams/*.html; do $$B --headless --screenshot="$${f%.html}.png" --window-size=1200,1000 "$$f" && echo "wrote $${f%.html}.png"; done
