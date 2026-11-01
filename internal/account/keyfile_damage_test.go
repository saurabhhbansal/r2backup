package account

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/saurabhhbansal/r2backup/internal/creds"
)

// A damaged key file -- truncated, edited, or just not what it claims --
// must come back as an error the person can read, never a crash or a
// machine brought to its knees by the cost parameters it carries.
func TestADamagedKeyFileIsAnErrorNotACrash(t *testing.T) {
	k := StoredKeys{Purpose: "s3", Primary: &creds.Credentials{Provider: creds.ProviderDrime, DrimeToken: "tok"}}
	good, err := ExportKeys("correct horse", k)
	if err != nil {
		t.Fatal(err)
	}
	damage := map[string]func(v map[string]any){
		"nonce cut short":  func(v map[string]any) { v["nonce"] = "AAAA" },
		"no nonce":         func(v map[string]any) { delete(v, "nonce") },
		"no passes":        func(v map[string]any) { v["kdf_params"].(map[string]any)["time"] = 0 },
		"no threads":       func(v map[string]any) { v["kdf_params"].(map[string]any)["threads"] = 0 },
		"no memory":        func(v map[string]any) { v["kdf_params"].(map[string]any)["memory_kib"] = 0 },
		"short key":        func(v map[string]any) { v["kdf_params"].(map[string]any)["key_len"] = 7 },
		"4 TiB of memory":  func(v map[string]any) { v["kdf_params"].(map[string]any)["memory_kib"] = uint32(1<<32 - 1) },
		"a billion passes": func(v map[string]any) { v["kdf_params"].(map[string]any)["time"] = 1_000_000_000 },
		"salt gone":        func(v map[string]any) { v["salt"] = "" },
	}
	for name, edit := range damage {
		t.Run(name, func(t *testing.T) {
			var f map[string]any
			if err := json.Unmarshal(good, &f); err != nil {
				t.Fatal(err)
			}
			edit(f["vault"].(map[string]any))
			data, _ := json.Marshal(f)
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("import panicked: %v", r)
				}
			}()
			_, err := ImportKeys("correct horse", data)
			if err == nil {
				t.Fatal("a damaged file imported")
			}
			if !errors.Is(err, ErrDecryptFailed) && !errors.Is(err, ErrNotKeyFile) {
				t.Errorf("error %q is neither ErrDecryptFailed nor ErrNotKeyFile", err)
			}
		})
	}
}
