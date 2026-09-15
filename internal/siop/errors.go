package siop

import (
	"errors"
	"fmt"
)

// ErrorCode is safe for callers to use when mapping verification failures to
// an HTTP response or an audit event. It never contains token material.
type ErrorCode string

const (
	ErrorMalformedToken       ErrorCode = "malformed_token"
	ErrorUnsupportedAlgorithm ErrorCode = "unsupported_algorithm"
	ErrorInvalidHeader        ErrorCode = "invalid_header"
	ErrorInvalidClaims        ErrorCode = "invalid_claims"
	ErrorIssuerMismatch       ErrorCode = "issuer_mismatch"
	ErrorSubjectMismatch      ErrorCode = "subject_mismatch"
	ErrorAudienceMismatch     ErrorCode = "audience_mismatch"
	ErrorNonceMismatch        ErrorCode = "nonce_mismatch"
	ErrorTokenExpired         ErrorCode = "token_expired"
	ErrorTokenNotYetValid     ErrorCode = "token_not_yet_valid"
	ErrorKIDMismatch          ErrorCode = "kid_mismatch"
	ErrorResolverFailed       ErrorCode = "resolver_failed"
	ErrorSignatureInvalid     ErrorCode = "signature_invalid"
)

// VerificationError describes a caller-safe verification failure. Detail is
// intentionally limited to protocol field names and never includes the token,
// nonce, signature, or resolved URL.
type VerificationError struct {
	Code   ErrorCode
	Detail string
	Cause  error
}

func (e *VerificationError) Error() string {
	if e.Detail == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func (e *VerificationError) Unwrap() error { return e.Cause }

func verificationError(code ErrorCode, detail string, cause error) error {
	return &VerificationError{Code: code, Detail: detail, Cause: cause}
}

// ErrorCodeOf returns the stable code carried by a verification error.
func ErrorCodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var target *VerificationError
	if errors.As(err, &target) {
		return target.Code
	}
	return ErrorResolverFailed
}
