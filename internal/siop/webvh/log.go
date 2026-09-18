package webvh

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"

	"github.com/ic3software/vtafarm-api/internal/siop"
)

const scidPlaceholder = "{SCID}"

type logEntry struct {
	raw         map[string]json.RawMessage
	versionID   string
	version     uint64
	versionHash string
	versionTime time.Time
	parameters  map[string]json.RawMessage
	state       json.RawMessage
	stateID     string
	proof       dataIntegrityProof
}

type dataIntegrityProof struct {
	raw                map[string]json.RawMessage
	verificationMethod string
	proofValue         string
	created            *time.Time
}

type parameterState struct {
	scid          string
	updateKeys    []string
	nextKeyHashes []string
	preRotation   bool
	portable      bool
	deactivated   bool
}

func validateLog(content []byte, requestedDID string, now time.Time, clockSkew time.Duration) ([]byte, error) {
	if len(content) == 0 || clockSkew < 0 {
		return nil, errors.New("empty DID log")
	}
	requestedSCID, err := scidFromDID(requestedDID)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(content), "\n")
	entries := make([]logEntry, 0, len(lines))
	for lineNumber, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		entry, err := parseLogEntry([]byte(line), now, clockSkew)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber+1, err)
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, errors.New("empty DID log")
	}

	var active parameterState
	var previous *logEntry
	matchedRequestedDID := false
	for index := range entries {
		entry := &entries[index]
		if entry.version != uint64(index+1) {
			return nil, errors.New("versionId sequence is not contiguous")
		}
		entrySCID, err := scidFromDID(entry.stateID)
		if err != nil || entrySCID != requestedSCID {
			return nil, errors.New("DID document SCID does not match requested DID")
		}
		if entry.stateID == requestedDID {
			matchedRequestedDID = true
		}
		if previous != nil && !entry.versionTime.After(previous.versionTime) {
			return nil, errors.New("versionTime is not strictly increasing")
		}
		if active.deactivated {
			return nil, errors.New("DID log has entries after deactivation")
		}

		previousState := active
		active, err = applyParameters(entry.parameters, previousState, previous != nil)
		if err != nil {
			return nil, fmt.Errorf("version %d parameters: %w", entry.version, err)
		}
		if active.scid != requestedSCID {
			return nil, errors.New("parameters.scid does not match requested DID")
		}

		authorizedKeys := active.updateKeys
		if previous != nil && !previousState.preRotation {
			authorizedKeys = previousState.updateKeys
		}
		if !proofKeyAuthorized(entry.proof.verificationMethod, authorizedKeys) {
			return nil, errors.New("log proof key is not authorized")
		}
		if err := verifyEntryProof(*entry); err != nil {
			return nil, err
		}
		if err := verifyEntryHash(*entry, previous); err != nil {
			return nil, err
		}
		if index == 0 {
			if err := verifyGenesisSCID(*entry); err != nil {
				return nil, err
			}
		} else if entry.stateID != previous.stateID {
			if !active.portable || !stateAlsoKnownAs(entry.state, previous.stateID) {
				return nil, errors.New("DID move does not satisfy portability requirements")
			}
		}
		previous = entry
	}
	if !matchedRequestedDID || entries[len(entries)-1].stateID != requestedDID {
		return nil, errors.New("requested DID is not the current DID document id")
	}
	if active.deactivated {
		return nil, errors.New("DID is deactivated")
	}
	return entries[len(entries)-1].state, nil
}

func parseLogEntry(data []byte, now time.Time, clockSkew time.Duration) (logEntry, error) {
	if _, err := jsoncanonicalizer.Transform(data); err != nil {
		return logEntry{}, fmt.Errorf("log entry is not valid I-JSON: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return logEntry{}, err
	}
	for name := range raw {
		switch name {
		case "versionId", "versionTime", "parameters", "state", "proof":
		default:
			return logEntry{}, errors.New("log entry contains an unknown field")
		}
	}
	var entry logEntry
	entry.raw = raw
	if err := requiredJSON(raw, "versionId", &entry.versionID); err != nil {
		return logEntry{}, err
	}
	versionNumber, versionHash, ok := strings.Cut(entry.versionID, "-")
	if !ok || versionHash == "" {
		return logEntry{}, errors.New("invalid versionId")
	}
	entry.version, _ = strconv.ParseUint(versionNumber, 10, 32)
	if entry.version == 0 || strconv.FormatUint(entry.version, 10) != versionNumber {
		return logEntry{}, errors.New("invalid versionId number")
	}
	entry.versionHash = versionHash

	var versionTime string
	if err := requiredJSON(raw, "versionTime", &versionTime); err != nil {
		return logEntry{}, err
	}
	entry.versionTime, _ = time.Parse(time.RFC3339, versionTime)
	if entry.versionTime.IsZero() || entry.versionTime.Format(time.RFC3339) != versionTime || entry.versionTime.After(now.Add(clockSkew)) {
		return logEntry{}, errors.New("invalid or future versionTime")
	}
	if err := requiredJSON(raw, "parameters", &entry.parameters); err != nil {
		return logEntry{}, err
	}
	if err := requiredJSON(raw, "state", &entry.state); err != nil {
		return logEntry{}, err
	}
	var state struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(entry.state, &state); err != nil || state.ID == "" {
		return logEntry{}, errors.New("DID document has no id")
	}
	entry.stateID = state.ID

	var proofs []json.RawMessage
	if err := requiredJSON(raw, "proof", &proofs); err != nil || len(proofs) != 1 {
		return logEntry{}, errors.New("log entry must contain exactly one proof")
	}
	proof, err := parseProof(proofs[0], now, clockSkew)
	if err != nil {
		return logEntry{}, err
	}
	entry.proof = proof
	return entry, nil
}

func parseProof(data []byte, now time.Time, clockSkew time.Duration) (dataIntegrityProof, error) {
	if _, err := jsoncanonicalizer.Transform(data); err != nil {
		return dataIntegrityProof{}, errors.New("proof is not valid I-JSON")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return dataIntegrityProof{}, err
	}
	for name := range raw {
		switch name {
		case "type", "cryptosuite", "created", "verificationMethod", "proofPurpose", "proofValue", "@context":
		default:
			return dataIntegrityProof{}, errors.New("proof contains an unknown field")
		}
	}
	var proofType, cryptosuite, purpose string
	var proof dataIntegrityProof
	proof.raw = raw
	if requiredJSON(raw, "type", &proofType) != nil || proofType != "DataIntegrityProof" ||
		requiredJSON(raw, "cryptosuite", &cryptosuite) != nil || cryptosuite != "eddsa-jcs-2022" ||
		requiredJSON(raw, "proofPurpose", &purpose) != nil || purpose != "assertionMethod" ||
		requiredJSON(raw, "verificationMethod", &proof.verificationMethod) != nil ||
		requiredJSON(raw, "proofValue", &proof.proofValue) != nil {
		return dataIntegrityProof{}, errors.New("proof has an invalid required field")
	}
	if createdRaw, ok := raw["created"]; ok {
		var created string
		if json.Unmarshal(createdRaw, &created) != nil {
			return dataIntegrityProof{}, errors.New("proof created is invalid")
		}
		parsed, err := time.Parse(time.RFC3339, created)
		if err != nil || parsed.Format(time.RFC3339) != created || parsed.After(now.Add(clockSkew)) {
			return dataIntegrityProof{}, errors.New("proof created is invalid or in the future")
		}
		proof.created = &parsed
	}
	return proof, nil
}

func applyParameters(raw map[string]json.RawMessage, previous parameterState, hasPrevious bool) (parameterState, error) {
	for name := range raw {
		switch name {
		case "method", "scid", "updateKeys", "portable", "nextKeyHashes", "witness", "watchers", "deactivated", "ttl":
		default:
			return parameterState{}, errors.New("unknown DID method parameter")
		}
	}
	current := previous
	current.updateKeys = slices.Clone(previous.updateKeys)
	current.nextKeyHashes = slices.Clone(previous.nextKeyHashes)
	if methodRaw, ok := raw["method"]; ok {
		var method string
		if json.Unmarshal(methodRaw, &method) != nil || method != "did:webvh:1.0" {
			return parameterState{}, errors.New("unsupported DID method version")
		}
	} else if !hasPrevious {
		return parameterState{}, errors.New("genesis entry has no method")
	}
	if scidRaw, ok := raw["scid"]; ok {
		if hasPrevious || json.Unmarshal(scidRaw, &current.scid) != nil || current.scid == "" {
			return parameterState{}, errors.New("invalid scid parameter")
		}
	} else if !hasPrevious {
		return parameterState{}, errors.New("genesis entry has no scid")
	}

	previousPreRotation := previous.preRotation
	if hashesRaw, ok := raw["nextKeyHashes"]; ok {
		if err := json.Unmarshal(hashesRaw, &current.nextKeyHashes); err != nil {
			return parameterState{}, errors.New("invalid nextKeyHashes")
		}
		if hasDuplicates(current.nextKeyHashes) {
			return parameterState{}, errors.New("nextKeyHashes contains duplicates")
		}
		for _, hash := range current.nextKeyHashes {
			if _, err := parseSHA256Multihash(hash); err != nil {
				return parameterState{}, errors.New("nextKeyHashes contains an invalid SHA-256 multihash")
			}
		}
		current.preRotation = len(current.nextKeyHashes) > 0
	} else if previousPreRotation {
		return parameterState{}, errors.New("nextKeyHashes must be present during pre-rotation")
	}

	if keysRaw, ok := raw["updateKeys"]; ok {
		var keys []string
		if err := json.Unmarshal(keysRaw, &keys); err != nil {
			return parameterState{}, errors.New("invalid updateKeys")
		}
		if len(keys) == 0 {
			if !hasPrevious || previousPreRotation {
				return parameterState{}, errors.New("updateKeys cannot be empty here")
			}
		} else {
			if hasDuplicates(keys) {
				return parameterState{}, errors.New("updateKeys contains duplicates")
			}
			for _, key := range keys {
				if _, err := decodeUpdateKey(key); err != nil {
					return parameterState{}, err
				}
			}
			if previousPreRotation {
				for _, key := range keys {
					if !contains(previous.nextKeyHashes, hashMultibaseString(key)) {
						return parameterState{}, errors.New("updateKey was not pre-committed")
					}
				}
			}
			current.updateKeys = keys
		}
	} else if !hasPrevious || previousPreRotation {
		return parameterState{}, errors.New("updateKeys must be present here")
	}
	if len(current.updateKeys) == 0 {
		return parameterState{}, errors.New("no active update keys")
	}

	if portableRaw, ok := raw["portable"]; ok {
		var portable bool
		if json.Unmarshal(portableRaw, &portable) != nil || (hasPrevious && portable) {
			return parameterState{}, errors.New("portable may only be enabled in genesis")
		}
		current.portable = portable
	}
	if witnessRaw, ok := raw["witness"]; ok {
		var witness map[string]json.RawMessage
		if json.Unmarshal(witnessRaw, &witness) != nil {
			return parameterState{}, errors.New("invalid witness parameter")
		}
		if len(witness) != 0 {
			return parameterState{}, errors.New("witnessed DID logs are not supported")
		}
	}
	if watchersRaw, ok := raw["watchers"]; ok {
		var watchers []string
		if json.Unmarshal(watchersRaw, &watchers) != nil {
			return parameterState{}, errors.New("invalid watchers")
		}
	}
	if ttlRaw, ok := raw["ttl"]; ok {
		var ttl uint32
		if json.Unmarshal(ttlRaw, &ttl) != nil {
			return parameterState{}, errors.New("invalid ttl")
		}
	}
	if deactivatedRaw, ok := raw["deactivated"]; ok {
		var deactivated bool
		if json.Unmarshal(deactivatedRaw, &deactivated) != nil || (!hasPrevious && deactivated) {
			return parameterState{}, errors.New("invalid deactivated parameter")
		}
		current.deactivated = deactivated
	}
	return current, nil
}

func verifyEntryProof(entry logEntry) error {
	proofConfig := cloneRawMap(entry.proof.raw)
	delete(proofConfig, "proofValue")
	proofCanonical, err := canonicalJSON(proofConfig)
	if err != nil {
		return errors.New("canonicalize proof config")
	}
	document := cloneRawMap(entry.raw)
	delete(document, "proof")
	documentCanonical, err := canonicalJSON(document)
	if err != nil {
		return errors.New("canonicalize log entry")
	}
	proofHash := sha256.Sum256(proofCanonical)
	documentHash := sha256.Sum256(documentCanonical)
	message := append(proofHash[:], documentHash[:]...)

	did, fragment, ok := strings.Cut(entry.proof.verificationMethod, "#")
	if !ok || !strings.HasPrefix(did, "did:key:") || fragment != strings.TrimPrefix(did, "did:key:") {
		return errors.New("proof verificationMethod is not a canonical did:key")
	}
	publicKey, err := (siop.DIDKeyResolver{}).ResolveAuthenticationKey(
		context.Background(), did, entry.proof.verificationMethod,
	)
	if err != nil {
		return errors.New("resolve log proof key")
	}
	signature, err := decodeProofValue(entry.proof.proofValue)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, message, signature) {
		return errors.New("log proof signature verification failed")
	}
	return nil
}

func verifyEntryHash(entry logEntry, previous *logEntry) error {
	working := cloneRawMap(entry.raw)
	delete(working, "proof")
	if previous == nil {
		var scid string
		_ = json.Unmarshal(entry.parameters["scid"], &scid)
		working["versionId"], _ = json.Marshal(scid)
	} else {
		working["versionId"], _ = json.Marshal(previous.versionID)
	}
	hash, err := hashJSON(working)
	if err != nil || hash != entry.versionHash {
		return errors.New("versionId hash does not match log entry")
	}
	return nil
}

func verifyGenesisSCID(entry logEntry) error {
	var scid string
	if json.Unmarshal(entry.parameters["scid"], &scid) != nil {
		return errors.New("invalid genesis scid")
	}
	working := cloneRawMap(entry.raw)
	delete(working, "proof")
	working["versionId"], _ = json.Marshal(scidPlaceholder)
	serialized, err := json.Marshal(working)
	if err != nil {
		return err
	}
	serialized = []byte(strings.ReplaceAll(string(serialized), scid, scidPlaceholder))
	canonical, err := jsoncanonicalizer.Transform(serialized)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	calculated := siop.EncodeBase58BTC(append([]byte{0x12, 0x20}, digest[:]...))
	if calculated != scid {
		return errors.New("genesis SCID does not match log entry")
	}
	return nil
}

func requiredJSON[T any](object map[string]json.RawMessage, name string, destination *T) error {
	raw, ok := object[name]
	if !ok || json.Unmarshal(raw, destination) != nil {
		return fmt.Errorf("missing or invalid %s", name)
	}
	return nil
}

func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return jsoncanonicalizer.Transform(encoded)
}

func hashJSON(value any) (string, error) {
	canonical, err := canonicalJSON(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return siop.EncodeBase58BTC(append([]byte{0x12, 0x20}, digest[:]...)), nil
}

func hashMultibaseString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return siop.EncodeBase58BTC(append([]byte{0x12, 0x20}, digest[:]...))
}

func parseSHA256Multihash(value string) ([]byte, error) {
	decoded, err := siop.DecodeBase58BTC(value)
	if err != nil || siop.EncodeBase58BTC(decoded) != value || len(decoded) != 34 || decoded[0] != 0x12 || decoded[1] != 0x20 {
		return nil, errors.New("value is not a canonical SHA-256 multihash")
	}
	return decoded[2:], nil
}

func decodeProofValue(value string) ([]byte, error) {
	if !strings.HasPrefix(value, "z") {
		return nil, errors.New("proofValue is not base58btc multibase")
	}
	decoded, err := siop.DecodeBase58BTC(value[1:])
	if err != nil || siop.EncodeBase58BTC(decoded) != value[1:] {
		return nil, errors.New("proofValue is not canonical base58btc")
	}
	return decoded, nil
}

func decodeUpdateKey(value string) (ed25519.PublicKey, error) {
	did := "did:key:" + value
	return (siop.DIDKeyResolver{}).ResolveAuthenticationKey(context.Background(), did, did+"#"+value)
}

func proofKeyAuthorized(verificationMethod string, keys []string) bool {
	did, fragment, ok := strings.Cut(verificationMethod, "#")
	if !ok || !strings.HasPrefix(did, "did:key:") || strings.TrimPrefix(did, "did:key:") != fragment {
		return false
	}
	return contains(keys, fragment)
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func hasDuplicates(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func cloneRawMap(source map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func scidFromDID(did string) (string, error) {
	rest, ok := strings.CutPrefix(did, "did:webvh:")
	if !ok {
		return "", errors.New("not a did:webvh DID")
	}
	scid, _, ok := strings.Cut(rest, ":")
	if !ok || scid == "" {
		return "", errors.New("did:webvh DID has no SCID")
	}
	if _, err := parseSHA256Multihash(scid); err != nil {
		return "", errors.New("did:webvh DID has an invalid SCID")
	}
	return scid, nil
}

func stateAlsoKnownAs(state json.RawMessage, wanted string) bool {
	var document struct {
		AlsoKnownAs []string `json:"alsoKnownAs"`
	}
	if json.Unmarshal(state, &document) != nil {
		return false
	}
	return contains(document.AlsoKnownAs, wanted)
}
