package passwordauth

import (
	"encoding/base64"
	"fmt"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestHashVerifyAndValidation(t *testing.T) {
	encoded, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if ok, upgrade := Verify(encoded, "correct horse battery staple"); !ok || upgrade {
		t.Fatalf("current credential ok=%v upgrade=%v", ok, upgrade)
	}
	if ok, _ := Verify(encoded, "wrong password entirely"); ok {
		t.Fatal("wrong password verified")
	}
	for _, invalid := range []string{"short", string(make([]byte, 257)), "valid length\x00but nul"} {
		if err := Validate(invalid); err == nil {
			t.Fatalf("accepted invalid password %q", invalid)
		}
	}
}

func TestVerifyRejectsUnboundedParametersAndDetectsUpgrade(t *testing.T) {
	salt := []byte("0123456789abcdef")
	password := "correct horse battery staple"
	key := argon2.IDKey([]byte(password), salt, 1, 8*1024, 1, 32)
	legacy := fmt.Sprintf("$argon2id$v=19$m=8192,t=1,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
	if ok, upgrade := Verify(legacy, password); !ok || !upgrade {
		t.Fatalf("legacy credential ok=%v upgrade=%v", ok, upgrade)
	}
	for _, encoded := range []string{
		"$argon2id$v=19$m=999999999,t=2,p=1$MDEyMzQ1Njc4OWFiY2RlZg$MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY",
		"$argon2id$v=19$m=19456,t=2,p=99$MDEyMzQ1Njc4OWFiY2RlZg$MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY",
		"not-a-credential",
	} {
		if ok, upgrade := Verify(encoded, password); ok || upgrade {
			t.Fatalf("unsafe credential accepted: %q", encoded)
		}
	}
}
