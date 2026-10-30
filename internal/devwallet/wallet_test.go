package devwallet

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceSeedReplacesPrivateSeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet", "seed")
	first, err := ReplaceSeed(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatalf("seed length=%d", len(first))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("seed mode=%o, want 600", got)
	}
	second, err := ReplaceSeed(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("existing development wallet seed was reused")
	}
	loaded, err := LoadSeed(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second, loaded) {
		t.Fatal("replacement development wallet seed was not persisted")
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
