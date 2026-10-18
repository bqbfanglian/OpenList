package feemoo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"
)

func TestOfficialWebCipherVector(t *testing.T) {
	plain := []byte(`{"filename":"中文 空格 &+.txt","totalSize":0}`)
	want := "fQaXb9ge2B/Hg61bhbMPFTVMlQrdRUpJQBA3PwlH3mZcQQ8As7zKKRRPMGicRbxnxqeQXZwAOQdHnIx34KMfTw=="
	encoded, err := encryptSecret(plain, "aBcDeF012345")
	if err != nil || encoded != want {
		t.Fatalf("web cipher mismatch: %v", err)
	}
	decoded, err := decryptSecret(want, "aBcDeF012345")
	if err != nil || !bytes.Equal(decoded, plain) {
		t.Fatal("web cipher decrypt failed")
	}
	for _, invalid := range []string{"", "?", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := decryptSecret(invalid, "test"); err == nil {
			t.Fatal("accepted malformed encrypted data")
		}
	}
}

func TestEncryptedResponseAndProtocolKeys(t *testing.T) {
	d := &Feemoo{}
	if err := d.loadProtocolKeys(); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	d.responseKey = key
	code := "testcode1234"
	ak, err := rsa.EncryptPKCS1v15(rand.Reader, &key.PublicKey, []byte(code))
	if err != nil {
		t.Fatal(err)
	}
	ed, err := encryptSecret([]byte(`{"total":"1"}`), code)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"ak": base64.StdEncoding.EncodeToString(ak), "ed": ed})
	decoded, err := d.decryptResponse(raw)
	if err != nil || string(decoded) != `{"total":"1"}` {
		t.Fatal("encrypted response decoding failed")
	}
}

func TestHashPartsKnownValues(t *testing.T) {
	d := &Feemoo{partSize: 4}
	p, err := d.prepare(context.Background(), source("test", []byte("abcdef"), true), func(float64) {})
	if err != nil {
		t.Fatal(err)
	}
	if p.hash != "fa40dffba3d56c6098e0477379f300bd" {
		t.Fatalf("aggregate hash %s", p.hash)
	}
	if p.fullMD5 != "e80b5017098950fc58aad83c8c14978e" {
		t.Fatal("full-file MD5 incorrect")
	}
	if len(p.parts) != 2 || p.parts[0] != "e2fc714c4727ee9395f324cd2e7f331f" {
		t.Fatal("part hashes incorrect")
	}
}

func TestSignedURLValidationAndErrorRedaction(t *testing.T) {
	for _, raw := range []string{"http://upload.example/a", "https://user:password@example.org/a", "file:///tmp/a"} {
		if secureUploadURL(raw) == nil {
			t.Fatal("unsafe upload URL accepted")
		}
	}
	err := transportError(context.Background(), &url.Error{Op: "Put", URL: "https://upload.example/part?secret=TEST_SECRET", Err: io.EOF})
	if err == nil || strings.Contains(err.Error(), "TEST_SECRET") {
		t.Fatal("signed URL leaked or request unexpectedly succeeded")
	}
}
