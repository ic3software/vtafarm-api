package model

import (
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

func TestSIOPDatabaseColumnNamesMatchMigration(t *testing.T) {
	tests := []struct {
		model  any
		fields map[string]string
	}{
		{UserSIOPIdentity{}, map[string]string{"DID": "did", "LastKID": "last_kid"}},
		{AdminSIOPIdentity{}, map[string]string{"DID": "did", "LastKID": "last_kid"}},
		{SIOPChallenge{}, map[string]string{"ExpectedDID": "expected_did", "NonceSHA256": "nonce_sha256"}},
	}
	for _, test := range tests {
		parsed, err := schema.Parse(test.model, &sync.Map{}, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		for fieldName, want := range test.fields {
			field := parsed.LookUpField(fieldName)
			if field == nil || field.DBName != want {
				got := "<missing>"
				if field != nil {
					got = field.DBName
				}
				t.Errorf("%T.%s column = %q, want %q", test.model, fieldName, got, want)
			}
		}
	}
}
