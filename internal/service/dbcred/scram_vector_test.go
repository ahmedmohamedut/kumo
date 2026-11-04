package dbcred

import "testing"

// The vector is not arbitrary. It is the fixture in
// packaging/images/postgres-18/verify.cloud-config.yaml, and M7.1 proved it
// against a real Postgres 18.6: a role created with
// `CREATE ROLE app LOGIN PASSWORD '<verifier>'` then authenticated the
// cleartext password over TCP with scram-sha-256.
//
// So this test is the standing check that the code still produces the string a
// real engine accepted. If it fails, the format drifted and no DB instance
// created afterwards would accept its own password -- which is why it is a
// known-answer test and not a round trip against the package itself.
const (
	vectorPassword = "kumo-verify-pw-2026"
	vectorSalt     = "kumoverifysalt16"
	vectorVerifier = "SCRAM-SHA-256$4096:a3Vtb3ZlcmlmeXNhbHQxNg==" +
		"$hiDQWkAbO/n9B7N0E+t3MMu7yW2/iznKlWrHTlhLak4=" +
		":6AV9UPNJrRLab9h/afybuQYged1b/uxraiwbnMww2sY="
)

func TestVerifierMatchesTheVectorPostgresAccepted(t *testing.T) {
	t.Parallel()

	got, err := verifierWithSalt(vectorPassword, []byte(vectorSalt))
	if err != nil {
		t.Fatalf("verifierWithSalt(): %v", err)
	}
	if got != vectorVerifier {
		t.Errorf("verifier mismatch\n got: %s\nwant: %s", got, vectorVerifier)
	}
}

// The salt is recorded in the verifier, so a client can derive without being
// told it. A verifier whose salt is not the one used would authenticate
// nothing.
func TestVerifierCarriesTheSaltItDerivedWith(t *testing.T) {
	t.Parallel()

	got, err := verifierWithSalt(vectorPassword, []byte(vectorSalt))
	if err != nil {
		t.Fatalf("verifierWithSalt(): %v", err)
	}
	// a3Vtb3ZlcmlmeXNhbHQxNg== is base64 of the salt above.
	if want := "SCRAM-SHA-256$4096:a3Vtb3ZlcmlmeXNhbHQxNg=="; got[:len(want)] != want {
		t.Errorf("prefix = %q, want %q", got[:len(want)], want)
	}
}
