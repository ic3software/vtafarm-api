package main

import (
	"bufio"
	"bytes"
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
	farmtrusttask "github.com/ic3software/vtafarm-api/internal/trusttask"
	prooflib "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/proof"
	authoobclaimv0_2 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/claim/v0_2"
	authoobgrantv0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/grant/v0_3"
	authoobidentifyv0_1 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/identify/v0_1"
	authoobprovev0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/prove/v0_3"
	authoobrespondv0_3 "github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/specs/auth/oob/respond/v0_3"
	"github.com/trustoverip/dtgwg-trust-tasks-tf/trust-tasks-go/trusttasks"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22,43}$`)

type config struct {
	apiBase        string
	requestID      string
	farmDID        string
	walletSeedFile string
	matchNumber    string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.apiBase, "api-base", "http://localhost:8080", "local VTA Farm API base URL")
	flag.StringVar(&cfg.requestID, "request-id", "", "sign-in request ID from the QR trigger link")
	flag.StringVar(&cfg.farmDID, "farm-did", "", "Farm DID from the QR trigger link")
	flag.StringVar(&cfg.walletSeedFile, "wallet-seed-file", devwallet.DefaultSeedFile, "private local seed created by the development VTA claim helper")
	flag.StringVar(&cfg.matchNumber, "match-number", "", "two-digit browser number; prompts when omitted")
	flag.Parse()

	if err := run(cfg, time.Now, &http.Client{Timeout: 15 * time.Second}, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "dev Keyring sign-in:", err)
		os.Exit(1)
	}
}

func run(cfg config, now func() time.Time, client *http.Client, input io.Reader, output io.Writer) error {
	endpoint, err := trustTaskEndpoint(cfg)
	if err != nil {
		return err
	}
	seed, err := devwallet.LoadSeed(cfg.walletSeedFile)
	if err != nil {
		return fmt.Errorf("load the identity created by make dev-claim: %w", err)
	}
	wallet, err := prooflib.SignerFromPrivateKey(prooflib.Ed25519, seed)
	if err != nil {
		return fmt.Errorf("create development wallet signer: %w", err)
	}

	claim := newDocument(authoobclaimv0_2.TypeURI, wallet.DID, cfg.farmDID, authoobclaimv0_2.Payload{RequestID: cfg.requestID}, now())
	claim.ParentThreadID = &cfg.requestID
	claimResponseBody, err := postSigned(client, endpoint, wallet, claim, "authentication", now())
	if err != nil {
		return fmt.Errorf("claim sign-in request: %w", err)
	}
	var claimed trusttasks.Document[authoobclaimv0_2.Response]
	if err := json.Unmarshal(claimResponseBody, &claimed); err != nil {
		return fmt.Errorf("decode claim response: %w", err)
	}
	if claimed.Payload.RequestID != cfg.requestID || claimed.Payload.Service.DID != cfg.farmDID {
		return errors.New("Farm returned a claim response for a different request or service")
	}

	fmt.Fprintln(output, "Development wallet DID:", wallet.DID)
	fmt.Fprintln(output, "Sign-in request claimed. The browser will show a two-digit number.")
	matchNumber := strings.TrimSpace(cfg.matchNumber)
	if matchNumber == "" {
		fmt.Fprint(output, "Enter the two-digit number shown in the browser: ")
		line, readErr := bufio.NewReader(input).ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read match number: %w", readErr)
		}
		matchNumber = strings.TrimSpace(line)
	}
	if !regexp.MustCompile(`^[0-9]{2}$`).MatchString(matchNumber) {
		return errors.New("match number must contain exactly two digits")
	}

	identify := newDocument(authoobidentifyv0_1.TypeURI, wallet.DID, cfg.farmDID, authoobidentifyv0_1.Payload{
		RequestID: cfg.requestID, ApproverKey: wallet.DID, EnteredNumber: matchNumber,
	}, now())
	identifyWire, err := signDocument(wallet, identify, "authentication", now())
	if err != nil {
		return fmt.Errorf("sign identity proof: %w", err)
	}
	var identifyObject map[string]json.RawMessage
	if err := json.Unmarshal(identifyWire, &identifyObject); err != nil {
		return fmt.Errorf("encode identity proof: %w", err)
	}
	prove := newDocument(authoobprovev0_3.TypeURI, wallet.DID, cfg.farmDID, authoobprovev0_3.Payload{Identify: identifyObject}, now())
	proveResponseBody, err := postSigned(client, endpoint, wallet, prove, "authentication", now())
	if err != nil {
		return fmt.Errorf("prove development wallet identity: %w", err)
	}
	var proved trusttasks.Document[authoobprovev0_3.Response]
	if err := json.Unmarshal(proveResponseBody, &proved); err != nil {
		return fmt.Errorf("decode prove response: %w", err)
	}
	if proved.Payload.RequestID != cfg.requestID || proved.Payload.IdentifiedAs != wallet.DID || proved.Payload.Service.DID != cfg.farmDID || proved.Payload.Origin != claimed.Payload.Origin {
		return errors.New("Farm returned a prove response with mismatched sign-in context")
	}
	contextDigest, err := farmtrusttask.DigestMultibase(json.RawMessage(proveResponseBody))
	if err != nil {
		return fmt.Errorf("digest prove response: %w", err)
	}

	grant := newDocument(authoobgrantv0_3.TypeURI, wallet.DID, cfg.farmDID, authoobgrantv0_3.Payload{
		RequestID:     cfg.requestID,
		Decision:      authoobgrantv0_3.PayloadDecisionApprove,
		SessionKey:    proved.Payload.SessionKey,
		ApproverKey:   wallet.DID,
		Origin:        proved.Payload.Origin,
		ContextDigest: contextDigest,
		SessionEndsAt: now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339),
		SigningScope:  proved.Payload.SigningScope,
	}, now())
	grantWire, err := signDocument(wallet, grant, "assertionMethod", now())
	if err != nil {
		return fmt.Errorf("sign approval grant: %w", err)
	}
	var grantObject map[string]json.RawMessage
	if err := json.Unmarshal(grantWire, &grantObject); err != nil {
		return fmt.Errorf("encode approval grant: %w", err)
	}
	respond := newDocument(authoobrespondv0_3.TypeURI, wallet.DID, cfg.farmDID, authoobrespondv0_3.Payload{Grant: grantObject}, now())
	respondBody, err := postSigned(client, endpoint, wallet, respond, "authentication", now())
	if err != nil {
		return fmt.Errorf("approve sign-in request: %w", err)
	}
	var responded trusttasks.Document[authoobrespondv0_3.Response]
	if err := json.Unmarshal(respondBody, &responded); err != nil {
		return fmt.Errorf("decode approval response: %w", err)
	}
	if responded.Payload.Status != authoobrespondv0_3.ResponseStatusApproved {
		return fmt.Errorf("Farm returned sign-in status %q", responded.Payload.Status)
	}

	fmt.Fprintln(output, "Approved. The original browser should now finish signing in.")
	return nil
}

func trustTaskEndpoint(cfg config) (string, error) {
	if !requestIDPattern.MatchString(cfg.requestID) {
		return "", errors.New("request ID must be 22-43 characters using letters, digits, '_' or '-'")
	}
	if !strings.HasPrefix(cfg.farmDID, "did:") {
		return "", errors.New("Farm DID must start with did:")
	}
	base, err := url.Parse(cfg.apiBase)
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", errors.New("API base must be an absolute URL without credentials, query or fragment")
	}
	hostname := base.Hostname()
	if hostname != "localhost" && hostname != "127.0.0.1" && hostname != "::1" {
		return "", errors.New("API base must point to localhost")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/trust-task/trust-tasks"
	return base.String(), nil
}

func newDocument[P any](typeURI, issuer, recipient string, payload P, at time.Time) *trusttasks.Document[P] {
	issuedAt := at.UTC().Truncate(time.Second).Format(time.RFC3339)
	expiresAt := at.UTC().Add(2 * time.Minute).Truncate(time.Second).Format(time.RFC3339)
	return &trusttasks.Document[P]{
		ID: "urn:uuid:" + uuid.NewString(), Type: typeURI, Issuer: &issuer, Recipient: &recipient,
		IssuedAt: &issuedAt, ExpiresAt: &expiresAt, Payload: payload,
	}
}

func signDocument[P any](signer *prooflib.Signer, document *trusttasks.Document[P], purpose string, at time.Time) ([]byte, error) {
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	signed, err := signer.Sign(encoded, prooflib.SignOptions{ProofPurpose: purpose, Created: at.UTC()})
	return []byte(signed), err
}

func postSigned[P any](client *http.Client, endpoint string, signer *prooflib.Signer, document *trusttasks.Document[P], purpose string, at time.Time) ([]byte, error) {
	body, err := signDocument(signer, document, purpose, at)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("API returned %s: %s", response.Status, strings.TrimSpace(string(responseBody)))
	}
	return responseBody, nil
}
