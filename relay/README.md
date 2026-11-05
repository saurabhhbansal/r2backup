# Ultra fast mode relay

A small Cloudflare Worker that copies a byte range from one URL to another without the bytes passing through the user's computer. Ultra fast mode uses it to copy Drime files to S3 storage cloud to cloud.

It is not deployed by the project. `r2b` uploads it to the user's own Cloudflare account, using an API token the user gives it, and binds a random secret that only that computer knows (`internal/ultrafast`, `internal/cfapi`). Turning the mode off deletes the Worker. r2backup decides what to copy and gives each request a Drime download link and a presigned S3 upload link, so the Worker holds no storage keys and keeps nothing.

- `src/relay.js`: the Worker, plain JavaScript with no build step. What the tests run is byte for byte what is uploaded.
- `relay.go`: embeds the script into the Go binary and carries `Version`, which must match the `VERSION` constant in `relay.js` (a Go test checks this). Bump both whenever the protocol or behaviour changes so installed copies are replaced.
- `test/relay.test.js`: vitest tests with fake fetch.

## Test

```
cd relay
npm ci
npm test
go test ./relay
```

CI runs both (the Node tests in the `relay` job, the Go test with the rest).
