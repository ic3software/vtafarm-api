package siop

import "testing"

func TestUnverifiedNonceUsesStrictTokenParser(t *testing.T) {
	identity := newTestIdentity(t, 31)
	token := mintToken(t, identity, nil, nil)
	nonce, err := UnverifiedNonce(token)
	if err != nil {
		t.Fatal(err)
	}
	if nonce != testNonce {
		t.Fatalf("nonce = %q, want %q", nonce, testNonce)
	}
	if _, err := UnverifiedNonce(token + ".extra"); err == nil {
		t.Fatal("accepted a token with four compact segments")
	}
}
