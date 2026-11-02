// Package relay carries the Worker that Ultra fast mode puts in the user's
// own Cloudflare account. The script is plain JavaScript with no build step,
// so what is tested in this folder is byte for byte what is uploaded.
package relay

import _ "embed"

// Script is the Worker module, uploaded as-is.
//
//go:embed src/relay.js
var Script []byte

// Version is the relay's VERSION constant, which its /health answer reports.
// A test keeps the two in step.
const Version = "1"
