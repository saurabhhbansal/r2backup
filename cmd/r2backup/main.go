// Command r2backup backs up folders to Cloudflare R2 and restores them anywhere.
package main

import "fmt"

// version is overwritten at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	fmt.Printf("r2backup %s\n", version)
}
