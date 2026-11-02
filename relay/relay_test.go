package relay

import (
	"strings"
	"testing"
)

func TestVersionMatchesScript(t *testing.T) {
	want := `export const VERSION = "` + Version + `";`
	if !strings.Contains(string(Script), want) {
		t.Fatalf("relay.js does not say %s; bump both together", want)
	}
}
