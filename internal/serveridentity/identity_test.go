package serveridentity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"golang.org/x/crypto/hkdf"
)

func TestServerIdentityKnownAnswer(t *testing.T) {
	key := make([]byte, 32)
	challenge := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
		challenge[i] = byte(i + 32)
	}
	seed := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, key, nil, []byte("pad-server-identity-ed25519-v1")), seed); err != nil {
		t.Fatal(err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(priv, Message(pub, challenge))
	for name, pair := range map[string][2]string{
		"seed":        {hex.EncodeToString(seed), "8c8346ca5da1a4799127ba98788225abc169fc5923a75cf2e502268ed9a849ef"},
		"public_key":  {hex.EncodeToString(pub), "a3383163144f101b00c64da51cc890afa84413255dc552b5e8e025b7389828ec"},
		"signature":   {hex.EncodeToString(sig), "efbb59aed8b78429f45e4e39a96155f2af5daa2ccf6336a43945adb0738b8bb8c46c6d5c247bbab53b4b312e628eeb48368efd7bdc4ac324074e5b419339c301"},
		"fingerprint": {Fingerprint(pub), "ee57-02d8-a6c5-d1cd-6fc4-249a-e07b-da2a"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %s, want %s", name, pair[0], pair[1])
		}
	}
	if !Verify(pub, challenge, sig) {
		t.Fatal("valid signature rejected")
	}
	for _, n := range []int{0, 31, 33, 63, 65} {
		if Verify(make([]byte, n), challenge, sig) || Verify(pub, make([]byte, n), sig) || Verify(pub, challenge, make([]byte, n)) {
			t.Fatalf("accepted invalid length %d", n)
		}
	}
	if Verify(pub, bytes.Repeat([]byte{1}, 32), sig) {
		t.Fatal("accepted wrong challenge")
	}
}
