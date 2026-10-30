package devwallet

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateSeedPersistsPrivateSeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet", "seed")
	first, created, err := LoadOrCreateSeed(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created || len(first) != 32 {
		t.Fatalf("created=%v seed length=%d", created, len(first))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("seed mode=%o, want 600", got)
	}
	second, created, err := LoadOrCreateSeed(path)
	if err != nil {
		t.Fatal(err)
	}
	if created || !bytes.Equal(first, second) {
		t.Fatal("existing development wallet seed was not reused")
	}
}

func TestLoadSeedRejectsBroadPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed")
	if err := os.WriteFile(path, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSeed(path); err == nil {
		t.Fatal("accepted a development wallet seed readable by other users")
	}
}
