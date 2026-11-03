package t3

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPair(t *testing.T) {
	f := newFake(t)
	tok, err := Pair(context.Background(), nil, f.base()+"/", fakeCode, "full-access", "Rubi Home")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Secret() != fakeToken || tok.ClientID != "client-1" {
		t.Errorf("token = %v", tok)
	}
	if want := []string{"orchestration:read", "orchestration:operate"}; !reflect.DeepEqual(tok.Scopes, want) {
		t.Errorf("scopes = %v", tok.Scopes)
	}
	if d := time.Until(tok.Expires); d < 50*time.Minute || d > 61*time.Minute {
		t.Errorf("expires in %v", d)
	}
}

func TestPairRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(f *fakeT3)
		code string
		msg  string
	}{
		"wrong code":      {code: "NOPE-1234", msg: "is not valid"},
		"other issuer":    {code: fakeCode, set: func(f *fakeT3) { f.issuer = "http://elsewhere.invalid" }, msg: "different issuer"},
		"state is lost":   {code: fakeCode, set: func(f *fakeT3) { f.badState = true }, msg: "state"},
		"wrong iss":       {code: fakeCode, set: func(f *fakeT3) { f.badIss = true }, msg: "issuer"},
		"endpoint abroad": {code: fakeCode, set: func(f *fakeT3) { f.foreignToken = true }, msg: "outside"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			if tc.set != nil {
				f.set(func() { tc.set(f) })
			}
			tok, err := Pair(context.Background(), nil, f.base(), tc.code, "full-access", "Rubi Home")
			if err == nil {
				t.Fatal("signed in")
			}
			if tok.Secret() != "" {
				t.Error("a refused sign-in returned a token")
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("error %q lacks %q", err, tc.msg)
			}
			if strings.Contains(err.Error(), tc.code) || strings.Contains(err.Error(), fakeToken) {
				t.Errorf("error leaks a secret: %v", err)
			}
		})
	}
}

func TestTokenIsNeverPrinted(t *testing.T) {
	tok := Token{Access: fakeToken, Scopes: []string{"a"}, ClientID: "c"}
	b, _ := json.Marshal(tok)
	b2, _ := json.Marshal(struct{ T Token }{tok})
	b3, _ := tok.MarshalText()
	for name, s := range map[string]string{
		"%v": fmt.Sprintf("%v", tok), "%+v": fmt.Sprintf("%+v", tok), "%s": fmt.Sprintf("%s", tok),
		"%#v": fmt.Sprintf("%#v", tok), "ptr": fmt.Sprintf("%v", &tok), "json": string(b), "nested json": string(b2),
		"text": string(b3), "Sprint": fmt.Sprint(tok),
	} {
		if strings.Contains(s, fakeToken) {
			t.Errorf("%s prints the token: %s", name, s)
		}
	}
}

func TestSaveAndLoadToken(t *testing.T) {
	tok := Token{Access: fakeToken, Scopes: []string{"a", "b"}, Expires: time.Unix(1_800_000_000, 0).UTC(), ClientID: "c"}
	b, err := tok.Save()
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadToken(b)
	if err != nil || !reflect.DeepEqual(got, tok) {
		t.Errorf("round trip: %v, %v", got, err)
	}
	for _, bad := range []string{"", "{", `{"access_token":""}`} {
		if _, err := LoadToken([]byte(bad)); err == nil {
			t.Errorf("LoadToken(%q) accepted", bad)
		}
	}
}

func TestPairDoesNotFollowRedirects(t *testing.T) {
	var hits int
	var body []byte
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		body, _ = io.ReadAll(r.Body)
	}))
	defer other.Close()
	f := newFake(t)
	f.set(func() { f.redirectDecision = other.URL + "/steal" })
	// Even a client that is set up to follow redirects must not.
	hc := &http.Client{}
	if _, err := Pair(context.Background(), hc, f.base(), fakeCode, "full-access", "Rubi Home"); err == nil {
		t.Fatal("signed in through a redirect")
	}
	if hits != 0 || strings.Contains(string(body), fakeCode) {
		t.Errorf("the other server got %d requests, body %q", hits, body)
	}
	if hc.CheckRedirect != nil {
		t.Error("Pair changed the caller's client")
	}
}

func TestNoRedirectsGivesATimeout(t *testing.T) {
	if c := noRedirects(nil); c.Timeout != oauthTimeout || c == http.DefaultClient {
		t.Errorf("client = %+v", c)
	}
	if c := noRedirects(&http.Client{Timeout: time.Second}); c.Timeout != time.Second {
		t.Error("the caller's timeout was replaced")
	}
}
