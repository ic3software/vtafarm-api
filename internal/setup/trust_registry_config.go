package setup

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

var registrySection = regexp.MustCompile(`(?m)^[\t ]*\[registry\][\t ]*(?:#.*)?$`)
var tomlSection = regexp.MustCompile(`(?m)^[\t ]*\[`)
var registryDidSetting = regexp.MustCompile(`(?m)^[\t ]*did[\t ]*=.*$`)

// MergeRegistryConfig preserves unrelated settings and comments, and refuses to
// silently move a community from a registry it already uses.
func MergeRegistryConfig(content, did string) (string, error) {
	if !strings.HasPrefix(did, "did:webvh:") {
		return "", errors.New("registry must have a did:webvh identity")
	}
	var config map[string]any
	if err := toml.Unmarshal([]byte(content), &config); err != nil {
		return "", errors.New("invalid VTC TOML")
	}
	if existing, ok := config["registry"].(map[string]any); ok {
		if target, ok := existing["did"].(string); ok && target != "" && target != did {
			return "", errors.New("VTC already points to another registry")
		}
		if target, ok := existing["url"].(string); ok && target != "" {
			return "", errors.New("VTC has an external registry URL; reconcile it before enabling a managed registry")
		}
	}
	encodedDid, _ := json.Marshal(did)
	line := "did = " + string(encodedDid)
	section := registrySection.FindStringIndex(content)
	if section == nil {
		if _, exists := config["registry"]; exists {
			return "", errors.New("use a [registry] table instead of an inline registry configuration")
		}
		content = strings.TrimRight(content, "\n") + "\n\n[registry]\n" + line + "\n"
	} else {
		end := len(content)
		if next := tomlSection.FindStringIndex(content[section[1]:]); next != nil {
			end = section[1] + next[0]
		}
		body := content[section[1]:end]
		if registryDidSetting.MatchString(body) {
			body = registryDidSetting.ReplaceAllStringFunc(body, func(string) string { return line })
		} else {
			body = "\n" + line + body
		}
		content = content[:section[1]] + body + content[end:]
	}
	var check map[string]any
	if err := toml.Unmarshal([]byte(content), &check); err != nil {
		return "", errors.New("cannot merge registry into VTC TOML")
	}
	expected, ok := config["registry"].(map[string]any)
	if !ok {
		expected = map[string]any{}
		config["registry"] = expected
	}
	expected["did"] = did
	// Text ranges can resemble tables inside TOML multiline strings. Refuse
	// any edit that changes more than the parsed registry DID setting.
	if !reflect.DeepEqual(config, check) {
		return "", errors.New("cannot preserve custom VTC TOML while merging registry")
	}
	return content, nil
}

func RegistryReferral(document []byte, vtcDid, registryDid string) ([]byte, bool, error) {
	var doc map[string]any
	if json.Unmarshal(document, &doc) != nil || doc["id"] != vtcDid {
		return nil, false, errors.New("VTC DID document identity mismatch")
	}
	services, ok := doc["service"].([]any)
	if doc["service"] != nil && !ok {
		return nil, false, errors.New("VTC DID services must be an array")
	}
	found := false
	for _, entry := range services {
		service, ok := entry.(map[string]any)
		if !ok {
			return nil, false, errors.New("invalid VTC DID service")
		}
		isRegistry := service["type"] == "TrustRegistry"
		if types, ok := service["type"].([]any); ok {
			for _, kind := range types {
				isRegistry = isRegistry || kind == "TrustRegistry"
			}
		}
		if isRegistry || service["id"] == vtcDid+"#trust-registry" {
			endpoint, _ := service["serviceEndpoint"].(map[string]any)
			if !found && endpoint["uri"] == registryDid && service["type"] == "TrustRegistry" {
				found = true
				continue
			}
			return nil, false, errors.New("VTC DID already refers to another registry")
		}
	}
	if found {
		return document, false, nil
	}
	doc["service"] = append(services, map[string]any{"id": vtcDid + "#trust-registry", "type": "TrustRegistry", "serviceEndpoint": map[string]string{"uri": registryDid, "profile": "https://trustoverip.org/profiles/trqp/v2"}})
	encoded, err := json.Marshal(doc)
	return encoded, true, err
}
