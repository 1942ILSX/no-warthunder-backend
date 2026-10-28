# Local gameplay load test

`cmd/loadtest` creates real rooms through `POST /api/rooms`, opens their authenticated WebSocket connections, and sends valid sequenced movement/fire input. It continuously reads server state and replaces finished or disconnected sessions to sustain the requested number of players. It rotates through all ten levels.

The tool never submits a player ID, calls the leaderboard submission endpoint, or persists earned completion tokens. It measures the actual server, not an in-process substitute.

## Run

Start the backend in a separate terminal. Then, from the backend project root:

```sh
go run ./cmd/loadtest -url http://127.0.0.1:8080 -rooms 25 -duration 60s
```

Optional `-rate 10` through `-rate 20` changes the number of input messages per second per connected room (default 15). The room count is limited to 1–25 and duration to 1 second–30 minutes. `Ctrl+C` cancels HTTP requests, closes WebSockets, joins client goroutines, and prints the report.

For cleaner resource measurements, build both the server and load generator before running them. `go run` introduces a build process, which should not be counted as server CPU/RAM. Run the generator on a separate machine when measuring an actual deployment so its cost does not consume the server's quota.

Only loopback targets are accepted. To test a remote deployment you own, use an SSH/local port forward and pass the local forwarded address. No external/public load target is enabled by default.

## Output

Progress is written to stderr every five seconds. The final JSON report goes to stdout, so it can be captured independently:

```sh
go run ./cmd/loadtest -rooms 25 -duration 60s > load-report.json
```

The report includes:

- Peak/current/average connected room count and time spent at the full requested concurrency. A connection is counted after a valid `welcome` message, not merely after room reservation.
- Successful sessions, finished wins/losses, and restarts. Session durations are measured from welcome to finish/disconnect/cancellation and include sessions truncated when the test ends.
- API errors, connection errors, protocol errors, and capacity retries. A transient 503 while the old room is being released is reported separately as a capacity retry. Existing human sessions also reduce available test capacity.
- Received messages, state snapshots, received/sent application payload bytes, and snapshot rate per connected-room-second. Byte totals exclude HTTP, WebSocket framing, TCP, and TLS overhead.
- Current connected rooms should be zero in the final report because all test connections have been closed. The server may take a brief scheduling interval to release its matching room entries.

The process exits nonzero for invalid options or when no gameplay session could connect. Other counters remain visible in the report rather than silently being treated as success. Test an intentional 26th-room rejection in a dedicated server test; it is not part of this traffic generator.

## CPU/RAM and quota verification

This client deliberately does **not** manufacture CPU/RAM estimates. Sample the separately running server process or its container. On macOS, monitor the backend PID with Activity Monitor or `ps`; on Linux, measure the deployed container's CPU and memory counters, including throttling and memory outside the Go heap.

A successful local run on a Mac does not establish compliance with a Linux `0.5 CPU / 256 MB` limit. Repeat the test with those actual enforced limits and inspect simulation/tick latency as well as average CPU and memory. The naive movement pattern also provides traffic and lifecycle coverage, not proof that every adversarial gameplay state has been exercised.
