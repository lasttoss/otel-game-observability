.PHONY: up down logs demo traces test check

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

test:
	go test -race -count=1 ./...

check:
	gofmt -l . | tee /dev/stderr | wc -l | grep -qx 0
	go vet ./...
	go test -race -count=1 ./...
	docker compose config -q
