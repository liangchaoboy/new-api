package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifySupplyAccount(t *testing.T) {
	for _, tc := range []struct {
		name                                        string
		challenge, badPassword, interactive, cookie bool
		want                                        int
	}{
		{name: "bearer", want: 12}, {name: "cookie session", cookie: true, want: 12}, {name: "second factor", challenge: true}, {name: "incorrect password", badPassword: true}, {name: "interactive challenge", interactive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := []string{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case "/api/status":
					if tc.interactive {
						w.Write([]byte(`{"success":true,"data":{"turnstile_check":true}}`))
						return
					}
					w.Write([]byte(`{"success":true,"data":{}}`))
				case "/api/user/login":
					var creds struct{ Username, Password string }
					assert.NoError(t, common.DecodeJson(r.Body, &creds))
					assert.Equal(t, "customer", creds.Username)
					if tc.badPassword {
						w.Write([]byte(`{"success":false}`))
						return
					}
					if tc.challenge {
						w.Write([]byte(`{"success":true,"data":{"require_2fa":true}}`))
						return
					}
					if tc.cookie {
						http.SetCookie(w, &http.Cookie{Name: "session", Value: "fixture", Path: "/"})
						w.Write([]byte(`{"success":true,"data":{"id":12}}`))
						return
					}
					w.Write([]byte(`{"success":true,"data":{"user":{"id":12},"access_token":"session-bearer"}}`))
				case "/api/user/self":
					assert.Equal(t, "12", r.Header.Get("New-Api-User"))
					if tc.cookie {
						_, err := r.Cookie("session")
						assert.NoError(t, err)
					} else {
						assert.Equal(t, "Bearer session-bearer", r.Header.Get("Authorization"))
					}
					w.Write([]byte(`{"success":true,"data":{"id":12,"username":"customer"}}`))
				case "/api/user/auth/logout", "/api/user/logout":
					w.Write([]byte(`{"success":true}`))
				default:
					t.Errorf("unexpected downstream request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			id, err := VerifySupplyAccount(t.Context(), srv.URL, "customer", "fixture-password")
			assert.Equal(t, tc.want, id)
			if tc.want == 0 {
				require.Error(t, err)
				assert.NotContains(t, paths, "/api/user/self")
			} else {
				require.NoError(t, err)
				assert.Len(t, paths, 4)
			}
		})
	}
}

func TestSupplyDeliveryProofBindsRecipientAndRevision(t *testing.T) {
	op := SupplyDeliveryContext{ResourceID: common.GetUUID(), Target: common.GetUUID(), Revision: 4}
	binding := func(v SupplyDeliveryContext) (VerificationBinding, error) {
		data, err := common.Marshal(v)
		require.NoError(t, err)
		return BindVerificationOperation(VerificationOperation{Scope: VerificationScopeSupplyDelivery, Context: data})
	}
	first, err := binding(op)
	require.NoError(t, err)
	changed := op
	changed.Target = common.GetUUID()
	other, err := binding(changed)
	require.NoError(t, err)
	assert.NotEqual(t, first, other)
	changed = op
	changed.Revision++
	other, err = binding(changed)
	require.NoError(t, err)
	assert.NotEqual(t, first, other)
	changed = op
	changed.ResourceID = ""
	_, err = binding(changed)
	assert.ErrorIs(t, err, ErrVerificationContextInvalid)
}
