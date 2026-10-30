package devwallet

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DefaultSeedFile = ".dev/keyring-wallet.seed"

func LoadSeed(path string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("development wallet seed path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read development wallet seed: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("development wallet seed %s must not be accessible by group or other users", path)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read development wallet seed: %w", err)
	}
	seed, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("development wallet seed must be standard base64 encoding of 32 bytes")
	}
	return seed, nil
}

func ReplaceSeed(path string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("development wallet seed path is empty")
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("generate development wallet seed: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create development wallet directory: %w", err)
	}
	file, err := os.CreateTemp(dir, ".keyring-wallet-seed-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary development wallet seed: %w", err)
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)
	encoded := base64.StdEncoding.EncodeToString(seed) + "\n"
	if _, err := file.WriteString(encoded); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write development wallet seed: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close development wallet seed: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return nil, fmt.Errorf("replace development wallet seed: %w", err)
	}
	return seed, nil
}
