package vault

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/didresolver"
)

func TrustRegistryPrefix(userID, sessionID uint) string {
	return fmt.Sprintf("trust-registry/user-%d/session-%d", userID, sessionID)
}

type RegistryCredential struct {
	Did                 string `json:"did"`
	PrivateKeyMultibase string `json:"privateKeyMultibase"`
	VtaDid              string `json:"vtaDid"`
	VtaURL              string `json:"vtaUrl"`
}

// RegistryCredential creates once with Vault CAS=0 so retries keep the ACL identity.
func (c *Client) RegistryCredential(ctx context.Context, userID, sessionID uint, vtaDid, vtaURL string) (RegistryCredential, error) {
	token, err := c.login(ctx)
	if err != nil {
		return RegistryCredential{}, err
	}
	path := "/v1/" + c.cfg.KVMount + "/data/" + TrustRegistryPrefix(userID, sessionID) + "/credential"
	read := func() (RegistryCredential, error) {
		var response struct {
			Data struct {
				Data map[string]string `json:"data"`
			} `json:"data"`
		}
		if err := c.do(ctx, http.MethodGet, path, token, nil, &response); err != nil {
			return RegistryCredential{}, err
		}
		var credential RegistryCredential
		if err := json.Unmarshal([]byte(response.Data.Data["credential"]), &credential); err != nil {
			return credential, errors.New("invalid registry credential in Vault")
		}
		if credential.Did == "" || credential.PrivateKeyMultibase == "" || credential.VtaDid != vtaDid {
			return credential, errors.New("registry credential does not match this VTA")
		}
		return credential, nil
	}
	credential, err := read()
	if err == nil {
		return credential, nil
	}
	var response *responseError
	if !errors.As(err, &response) || response.statusCode != http.StatusNotFound {
		return credential, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return credential, err
	}
	did, err := didkey.FromPublicKey(pub)
	if err != nil {
		return credential, err
	}
	credential = RegistryCredential{Did: did, PrivateKeyMultibase: "z" + didresolver.EncodeBase58BTC(append([]byte{0x80, 0x26}, priv.Seed()...)), VtaDid: vtaDid, VtaURL: vtaURL}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return RegistryCredential{}, err
	}
	if err := c.do(ctx, http.MethodPost, path, token, map[string]any{"options": map[string]int{"cas": 0}, "data": map[string]string{"credential": string(encoded)}}, nil); err != nil {
		// Another worker may have won the CAS; read its credential rather than rotating it.
		return read()
	}
	return credential, nil
}

func (c *Client) DeleteTrustRegistrySecrets(ctx context.Context, userID, sessionID uint) error {
	token, err := c.login(ctx)
	if err != nil {
		return err
	}
	return c.deleteSecretTree(ctx, token, TrustRegistryPrefix(userID, sessionID))
}
