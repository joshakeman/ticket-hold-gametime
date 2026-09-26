---
name: test
description: Run this repo's tests against the real Postgres database, with the race detector. Use whenever tests need to be run or a change needs verifying.
---

1. Make sure the DB is up: `make db` (Docker service `db`, host port 5433).
   If `docker` isn't on PATH, try `/Applications/Docker.app/Contents/Resources/bin/docker`.
2. Run the tests:

   ```bash
   TEST_DATABASE_URL='postgres://tickets:tickets@localhost:5433/tickets?sslmode=disable' go test -race -count=1 -v ./... 2>&1 | grep -E '^(---|ok|FAIL|PASS|panic)|_test.go'
   ```

   `make test` does the same thing without `-v`.
3. Check that tests actually ran. A `--- SKIP` or "TEST_DATABASE_URL not set"
   line means nothing was tested. Don't report a pass in that case.
4. Report each `--- PASS`/`--- FAIL` line. For a failure, show the assertion
   message and the file and line.

To run one test, add `-run 'TestName' ./internal/store/`. To check concurrency
tests for flakiness, add `-count=20`.
