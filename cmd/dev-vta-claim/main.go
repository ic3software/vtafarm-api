package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/devwallet"
	prooflib "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/proof"
	provisionvtaclaimv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/provision/vta/claim/v0_1"
	"github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/trusttasks"
)

var handlePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22,43}$`)

type config struct {
	apiBase        string
	handle         string
	farmDID        string
	walletSeedFile string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.apiBase, "api-base", "http://localhost:8080", "local VTA Farm API base URL")
	flag.StringVar(&cfg.handle, "handle", "", "claim ID from the QR trigger link")
	flag.StringVar(&cfg.farmDID, "farm-did", "", "Farm DID from the QR trigger link")
	flag.StringVar(&cfg.walletSeedFile, "wallet-seed-file", devwallet.DefaultSeedFile, "private local seed replaced by each development claim and loaded by the sign-in helper")
	flag.Parse()

	seed, err := devwallet.ReplaceSeed(cfg.walletSeedFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dev claim:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "Generated fresh private development wallet:", cfg.walletSeedFile)
	if err := run(cfg, time.Now, bytes.NewReader(seed), &http.Client{Timeout: 15 * time.Second}, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "dev claim:", err)
		os.Exit(1)
	}
}

func run(cfg config, now func() time.Time, random io.Reader, client *http.Client, output io.Writer) error {
	endpoint, err := claimEndpoint(cfg)
	if err != nil {
		return err
	}
	body, walletDID, claimCheck, err := signedClaim(cfg.handle, cfg.farmDID, now().UTC(), random)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send claim: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("API returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}

	fmt.Fprintln(output, "Claim started.")
	fmt.Fprintln(output, "Wallet DID:", walletDID)
	fmt.Fprintln(output, "Claim check:", claimCheck)
	return nil
}

func claimEndpoint(cfg config) (string, error) {
	if !handlePattern.MatchString(cfg.handle) {
		return "", errors.New("handle must be 22-43 characters using letters, digits, '_' or '-'")
	}
	if !strings.HasPrefix(cfg.farmDID, "did:") {
		return "", errors.New("farm DID must start with did:")
	}
	base, err := url.Parse(cfg.apiBase)
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", errors.New("api base must be an absolute URL without credentials, query or fragment")
	}
	hostname := base.Hostname()
	if hostname != "localhost" && hostname != "127.0.0.1" && hostname != "::1" {
		return "", errors.New("api base must point to localhost")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/trust-task/trust-tasks"
	return base.String(), nil
}

func signedClaim(handle, farmDID string, now time.Time, random io.Reader) ([]byte, string, string, error) {
	seed := make([]byte, 32)
	if _, err := io.ReadFull(random, seed); err != nil {
		return nil, "", "", fmt.Errorf("generate wallet key: %w", err)
	}
	signer, err := prooflib.SignerFromPrivateKey(prooflib.Ed25519, seed)
	if err != nil {
		return nil, "", "", fmt.Errorf("create wallet signer: %w", err)
	}
	issuedAt := now.Truncate(time.Second).Format(time.RFC3339)
	expiresAt := now.Add(2 * time.Minute).Truncate(time.Second).Format(time.RFC3339)
	document := trusttasks.Document[provisionvtaclaimv0_1.Payload]{
		ID:             "urn:uuid:" + uuid.NewString(),
		ParentThreadID: &handle,
		Type:           provisionvtaclaimv0_1.TypeURI,
		Issuer:         &signer.DID,
		Recipient:      &farmDID,
		IssuedAt:       &issuedAt,
		ExpiresAt:      &expiresAt,
		Payload:        provisionvtaclaimv0_1.Payload{Handle: handle},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, "", "", fmt.Errorf("encode claim: %w", err)
	}
	signed, err := signer.Sign(encoded, prooflib.SignOptions{ProofPurpose: "authentication", Created: now})
	if err != nil {
		return nil, "", "", fmt.Errorf("sign claim: %w", err)
	}
	digest := sha256.Sum256([]byte(signer.DID))
	claimCheck := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])[:6]
	return signed, signer.DID, claimCheck, nil
}
