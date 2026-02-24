package api_keys //nolint:testpackage // Testing private helper methods requires same package

import (
	"testing"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/token"
)

func TestIsAuthorizedForKey(t *testing.T) {
	tests := []struct {
		name     string
		user     *token.UserContext
		keyOwner string
		want     bool
	}{
		{
			name: "owner can access their own key",
			user: &token.UserContext{
				Username: "alice",
				Groups:   []string{"users"},
			},
			keyOwner: "alice",
			want:     true,
		},
		{
			name: "non-owner cannot access key",
			user: &token.UserContext{
				Username: "bob",
				Groups:   []string{"users"},
			},
			keyOwner: "alice",
			want:     false,
		},
		{
			name: "admin can access any key",
			user: &token.UserContext{
				Username: "admin-user",
				Groups:   []string{"admin-users", "users"},
			},
			keyOwner: "alice",
			want:     true,
		},
		{
			name: "admin with different username can access key",
			user: &token.UserContext{
				Username: "super-admin",
				Groups:   []string{"admin-users"},
			},
			keyOwner: "alice",
			want:     true,
		},
		{
			name: "user with no groups cannot access other's key",
			user: &token.UserContext{
				Username: "charlie",
				Groups:   []string{},
			},
			keyOwner: "alice",
			want:     false,
		},
		{
			name: "user with different groups cannot access other's key",
			user: &token.UserContext{
				Username: "dave",
				Groups:   []string{"developers", "qa-team"},
			},
			keyOwner: "alice",
			want:     false,
		},
		{
			name: "case-sensitive username match required",
			user: &token.UserContext{
				Username: "Alice",
				Groups:   []string{"users"},
			},
			keyOwner: "alice",
			want:     false,
		},
		{
			name: "empty username cannot access key",
			user: &token.UserContext{
				Username: "",
				Groups:   []string{"users"},
			},
			keyOwner: "alice",
			want:     false,
		},
		{
			name: "admin group check is case-sensitive",
			user: &token.UserContext{
				Username: "admin",
				Groups:   []string{"Admin-Users"}, // Wrong case
			},
			keyOwner: "alice",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{}
			got := h.isAuthorizedForKey(tt.user, tt.keyOwner)
			if got != tt.want {
				t.Errorf("isAuthorizedForKey() = %v, want %v", got, tt.want)
			}
		})
	}
}
