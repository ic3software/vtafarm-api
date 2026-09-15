package siop

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const maxCompactTokenBytes = 64 * 1024

type protectedHeader struct {
	Algorithm string
	KID       string
	Type      string
}

type parsedToken struct {
	header       protectedHeader
	claims       claims
	signingInput string
	signature    []byte
}

func parseCompactToken(compact string) (parsedToken, error) {
	if compact == "" || len(compact) > maxCompactTokenBytes {
		return parsedToken{}, verificationError(ErrorMalformedToken, "id_token has an invalid size", nil)
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return parsedToken{}, verificationError(ErrorMalformedToken, "id_token must have three non-empty compact JWS segments", nil)
	}

	headerBytes, err := decodeCanonicalBase64URL(parts[0])
	if err != nil {
		return parsedToken{}, verificationError(ErrorInvalidHeader, "protected header is not canonical base64url", err)
	}
	header, err := parseProtectedHeader(headerBytes)
	if err != nil {
		return parsedToken{}, err
	}

	payloadBytes, err := decodeCanonicalBase64URL(parts[1])
	if err != nil {
		return parsedToken{}, verificationError(ErrorInvalidClaims, "payload is not canonical base64url", err)
	}
	parsedClaims, err := parseClaims(payloadBytes)
	if err != nil {
		return parsedToken{}, err
	}

	signature, err := decodeCanonicalBase64URL(parts[2])
	if err != nil {
		return parsedToken{}, verificationError(ErrorMalformedToken, "signature is not canonical base64url", err)
	}

	return parsedToken{
		header:       header,
		claims:       parsedClaims,
		signingInput: parts[0] + "." + parts[1],
		signature:    signature,
	}, nil
}

func parseProtectedHeader(data []byte) (protectedHeader, error) {
	fields, err := decodeUniqueObject(data)
	if err != nil {
		return protectedHeader{}, verificationError(ErrorInvalidHeader, "protected header is not a unique-key JSON object", err)
	}
	for name := range fields {
		switch name {
		case "alg", "kid", "typ":
		default:
			return protectedHeader{}, verificationError(ErrorInvalidHeader, "protected header contains an unsupported field", nil)
		}
	}

	var header protectedHeader
	if err := decodeRequiredString(fields, "alg", &header.Algorithm); err != nil {
		return protectedHeader{}, verificationError(ErrorInvalidHeader, "protected header has an invalid alg", err)
	}
	if header.Algorithm != "EdDSA" {
		return protectedHeader{}, verificationError(ErrorUnsupportedAlgorithm, "only EdDSA is supported", nil)
	}
	if err := decodeRequiredString(fields, "kid", &header.KID); err != nil || header.KID == "" {
		return protectedHeader{}, verificationError(ErrorInvalidHeader, "protected header has an invalid kid", err)
	}
	if raw, ok := fields["typ"]; ok {
		if err := json.Unmarshal(raw, &header.Type); err != nil || header.Type != "JWT" {
			return protectedHeader{}, verificationError(ErrorInvalidHeader, "protected header typ must be JWT when present", err)
		}
	}
	return header, nil
}

func decodeCanonicalBase64URL(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, err
	}
	if base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, errors.New("non-canonical base64url encoding")
	}
	return decoded, nil
}

func decodeUniqueObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := first.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("value is not an object")
	}

	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("object field name is not a string")
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, fmt.Errorf("duplicate field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected trailing JSON token %v", token)
	}
	return fields, nil
}

func decodeRequiredString(fields map[string]json.RawMessage, name string, destination *string) error {
	raw, ok := fields[name]
	if !ok {
		return fmt.Errorf("missing %s", name)
	}
	if err := json.Unmarshal(raw, destination); err != nil {
		return err
	}
	if *destination == "" {
		return fmt.Errorf("empty %s", name)
	}
	return nil
}
