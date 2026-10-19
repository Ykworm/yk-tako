package runengine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testIssuer(ttl time.Duration) *tokenIssuer {
	return &tokenIssuer{secret: []byte("dev-only-jwt-secret-change-me-32bytes"), ttl: ttl}
}

func TestAccessTokenRoundTrip(t *testing.T) {
	iss := testIssuer(time.Minute)
	raw, exp, err := iss.issueAccessToken("u_1", "t_1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if exp != 60 {
		t.Fatalf("expiresIn=%d", exp)
	}
	sub, tid, ver, err := iss.parseAccessToken(raw)
	if err != nil {
		t.Fatal(err)
	}
	if sub != "u_1" || tid != "t_1" || ver != 3 {
		t.Fatalf("claims sub=%s tid=%s ver=%d", sub, tid, ver)
	}
}

func TestAccessTokenExpired(t *testing.T) {
	iss := testIssuer(-time.Minute)
	raw, _, err := iss.issueAccessToken("u_1", "t_1", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = iss.parseAccessToken(raw)
	if !errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("err=%v", err)
	}
}

func TestAccessTokenTampered(t *testing.T) {
	iss := testIssuer(time.Minute)
	raw, _, err := iss.issueAccessToken("u_1", "t_1", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = iss.parseAccessToken(raw + "x")
	if err == nil || errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("err=%v", err)
	}
	if strings.Count(raw, ".") != 2 {
		t.Fatalf("token segments: %s", raw)
	}
}
