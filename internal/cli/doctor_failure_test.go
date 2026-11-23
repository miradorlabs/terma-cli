package cli

import (
	"net/http"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/delivery"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// A refusal of what a profile sends with names its own credential: the server key on a
// server-key profile, the person's sign-in otherwise (a 403 for an organization they left),
// its token for a 401, and neither for a retired sign-in, which has its own fix.
func TestADoctorFailureNamesTheProfilesOwnCredential(t *testing.T) {
	t.Parallel()
	forbidden := delivery.Failure{ProjectID: "p1", Endpoint: "https://otlp.example", Err: &delivery.IngestError{Status: http.StatusForbidden, Detail: "ORG_NOT_ACCESSIBLE"}}
	if got := doctorFailure(forbidden, false); got.KeyRefused || !got.SignInRefused {
		t.Errorf("a person's refused sign-in = %+v", got)
	}
	if got := doctorFailure(forbidden, true); !got.KeyRefused || got.SignInRefused {
		t.Errorf("a refused server key = %+v", got)
	}
	unauthorized := forbidden
	unauthorized.Err = &delivery.IngestError{Status: http.StatusUnauthorized}
	if got := doctorFailure(unauthorized, false); got.KeyRefused || got.SignInRefused || !got.TokenRefused {
		t.Errorf("a person's refused token = %+v", got)
	}
	retired := forbidden
	retired.Err = &delivery.IngestError{Status: http.StatusForbidden, Reason: sendas.ReasonSignInRetired}
	if got := doctorFailure(retired, false); !got.Retired || got.KeyRefused || got.SignInRefused || got.TokenRefused {
		t.Errorf("a retired sign-in = %+v", got)
	}
}
