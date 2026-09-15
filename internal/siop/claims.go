package siop

import (
	"encoding/json"
	"fmt"
	"time"
)

type claims struct {
	Issuer    string
	Subject   string
	Audience  string
	Nonce     string
	IssuedAt  int64
	ExpiresAt int64
}

func parseClaims(data []byte) (claims, error) {
	fields, err := decodeUniqueObject(data)
	if err != nil {
		return claims{}, verificationError(ErrorInvalidClaims, "payload is not a unique-key JSON object", err)
	}
	for name := range fields {
		switch name {
		case "iss", "sub", "aud", "nonce", "iat", "exp":
		default:
			return claims{}, verificationError(ErrorInvalidClaims, "payload contains an unsupported claim", nil)
		}
	}

	var parsed claims
	for _, field := range []struct {
		name        string
		destination *string
	}{
		{"iss", &parsed.Issuer},
		{"sub", &parsed.Subject},
		{"aud", &parsed.Audience},
		{"nonce", &parsed.Nonce},
	} {
		if err := decodeRequiredString(fields, field.name, field.destination); err != nil {
			return claims{}, verificationError(ErrorInvalidClaims, fmt.Sprintf("payload has an invalid %s", field.name), err)
		}
	}
	if err := decodeRequiredInt64(fields, "iat", &parsed.IssuedAt); err != nil {
		return claims{}, verificationError(ErrorInvalidClaims, "payload has an invalid iat", err)
	}
	if err := decodeRequiredInt64(fields, "exp", &parsed.ExpiresAt); err != nil {
		return claims{}, verificationError(ErrorInvalidClaims, "payload has an invalid exp", err)
	}
	return parsed, nil
}

func decodeRequiredInt64(fields map[string]json.RawMessage, name string, destination *int64) error {
	raw, ok := fields[name]
	if !ok {
		return fmt.Errorf("missing %s", name)
	}
	if err := json.Unmarshal(raw, destination); err != nil {
		return err
	}
	if *destination < 0 {
		return fmt.Errorf("negative %s", name)
	}
	return nil
}

func (c claims) validate(expectedDID, audience, nonce string, now time.Time, clockSkew time.Duration) error {
	if c.Issuer != expectedDID {
		return verificationError(ErrorIssuerMismatch, "issuer does not match the expected DID", nil)
	}
	if c.Subject != c.Issuer {
		return verificationError(ErrorSubjectMismatch, "issuer and subject differ", nil)
	}
	if c.Audience != audience {
		return verificationError(ErrorAudienceMismatch, "audience does not match the relying party", nil)
	}
	if c.Nonce != nonce {
		return verificationError(ErrorNonceMismatch, "nonce does not match the challenge", nil)
	}
	if c.IssuedAt > c.ExpiresAt {
		return verificationError(ErrorInvalidClaims, "iat is after exp", nil)
	}

	skewSeconds := int64(clockSkew / time.Second)
	nowSeconds := now.Unix()
	if c.IssuedAt > nowSeconds+skewSeconds {
		return verificationError(ErrorTokenNotYetValid, "iat is in the future", nil)
	}
	if c.ExpiresAt <= nowSeconds-skewSeconds {
		return verificationError(ErrorTokenExpired, "id_token has expired", nil)
	}
	return nil
}
