package authn

import (
	"testing"

	account "github.com/nitecon/amux/identity/user"
)

func TestBrowserSessionTokenMatchesRustFixtureAndBindsAccountState(t *testing.T) {
	const rustToken = "7.10000.5fd0bb4c18b55e7977bca684101e934648e9d352ff44531d8d94f165b18d2cde"
	user := account.Account{ID: 7, SessionEpoch: 1}
	if got := MintBrowserSession("k", user, 10_000); got != rustToken {
		t.Fatalf("minted token=%q, want Rust fixture %q", got, rustToken)
	}
	if userID, ok := BrowserSessionUserID(rustToken); !ok || userID != 7 {
		t.Fatalf("token user id=%d present=%v", userID, ok)
	}
	for _, test := range []struct {
		name string
		key  string
		user account.Account
		now  int64
		want bool
	}{
		{name: "valid before expiry", key: "k", user: account.Account{ID: 7, SessionEpoch: 1}, now: 9_999, want: true},
		{name: "expiry boundary", key: "k", user: account.Account{ID: 7, SessionEpoch: 1}, now: 10_000},
		{name: "wrong signing key", key: "other", user: account.Account{ID: 7, SessionEpoch: 1}, now: 1},
		{name: "wrong user", key: "k", user: account.Account{ID: 8, SessionEpoch: 1}, now: 1},
		{name: "revoked epoch", key: "k", user: account.Account{ID: 7, SessionEpoch: 2}, now: 1},
		{name: "inactive account", key: "k", user: account.Account{ID: 7, SessionEpoch: 1, DisabledAt: browserSessionInt64Ptr(1)}, now: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := VerifyBrowserSession(test.key, test.user, rustToken, test.now); got != test.want {
				t.Fatalf("verify=%v, want %v", got, test.want)
			}
		})
	}
}

func TestBrowserSessionTokenRejectsMalformedAndNonCanonicalSignatures(t *testing.T) {
	user := account.Account{ID: 7, SessionEpoch: 1}
	for _, token := range []string{
		"", "garbage", "user.10000.sig", "7.expiry.sig", "7.10000", "7.10000." + "5FD0BB4C18B55E7977BCA684101E934648E9D352FF44531D8D94F165B18D2CDE",
		"7.10000.5fd0bb4c18b55e7977bca684101e934648e9d352ff44531d8d94f165b18d2cde.extra",
	} {
		if VerifyBrowserSession("k", user, token, 1) {
			t.Errorf("accepted malformed token %q", token)
		}
	}
	for token, want := range map[string]int64{"7.10000.sig": 7, "007.1.sig": 7, "+7.1.sig": 7, "-2.1.sig": -2} {
		if got, ok := BrowserSessionUserID(token); !ok || got != want {
			t.Errorf("token id %q => %d,%v want %d,true", token, got, ok, want)
		}
	}
	for _, token := range []string{"", ".1.sig", "x.1.sig"} {
		if _, ok := BrowserSessionUserID(token); ok {
			t.Errorf("extracted user id from %q", token)
		}
	}
}

func browserSessionInt64Ptr(value int64) *int64 { return &value }
