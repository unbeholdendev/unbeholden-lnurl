package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testDiagnostics(t *testing.T, n *network, binary, node, backend string) {
	credential := filepath.Join(t.TempDir(), "read-only-credential")
	switch backend {
	case "lnd":
		run(t, n.lnd, "bakemacaroon", "--save_to="+credential, "invoices:read")
		node = strings.Replace(node, filepath.Join(n.root, "backend.macaroon"), credential, 1)
	case "cln":
		rune := run(t, n.receiver, "-k", "createrune", `restrictions=[["method=listinvoices"]]`)["rune"].(string)
		write(t, credential, []byte(rune))
		node = strings.Replace(node, filepath.Join(n.root, "backend.rune"), credential, 1)
	}
	s := newService(t, binary, node, addressYAML)
	if err := os.MkdirAll(s.stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"settle_index":     "17\n",
		"zap_settle_index": "23\n",
		"state.json":       `{"claimed":{"existing":{"claimed_at":"2026-01-01T00:00:00Z","amount_msat":100000}}}`,
	} {
		write(t, filepath.Join(s.stateDir, name), []byte(data))
	}
	snapshot := func() map[string]string {
		t.Helper()
		files := map[string]string{}
		err := filepath.WalkDir(s.stateDir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	invoiceCount := func() int {
		t.Helper()
		base := n.receiver
		if backend == "lnd" {
			base = n.lnd
		}
		invoices := run(t, base, "listinvoices")["invoices"]
		if invoices == nil {
			return 0
		}
		return len(invoices.([]any))
	}
	beforeState, beforeInvoices := snapshot(), invoiceCount()
	for _, configPath := range []string{s.configPath, filepath.Join(t.TempDir(), "missing.yaml")} {
		t.Run(filepath.Base(configPath), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, binary, "-config", configPath, "-diagnose").CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("diagnostics timed out: %v\n%s", err, out)
			}
			if configPath == s.configPath {
				if err != nil || !strings.Contains(string(out), "OK node") {
					t.Fatalf("diagnostics with read-only credentials: %v\n%s", err, out)
				}
			} else if err == nil {
				t.Fatalf("diagnostics accepted missing config: %s", out)
			}
			if after := snapshot(); !reflect.DeepEqual(after, beforeState) {
				t.Errorf("diagnostics changed state: before %v, after %v", beforeState, after)
			}
			if after := invoiceCount(); after != beforeInvoices {
				t.Errorf("diagnostics changed invoice count: before %d, after %d", beforeInvoices, after)
			}
		})
	}
}
