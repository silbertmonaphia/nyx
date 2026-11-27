package access

import "testing"

func TestAllows(t *testing.T) {
	cases := []struct {
		name     string
		list     []string
		username string
		want     bool
	}{
		{"empty list allows anyone", nil, "alice", true},
		{"listed user is allowed", []string{"alice", "bob"}, "alice", true},
		{"unlisted user is denied", []string{"alice"}, "carol", false},
		{"matching is case-sensitive", []string{"alice"}, "Alice", false},
		{"empty username with non-empty list is denied", []string{"alice"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Allows(tc.list, tc.username); got != tc.want {
				t.Errorf("Allows(%v, %q) = %t, want %t", tc.list, tc.username, got, tc.want)
			}
		})
	}
}

func TestRegistrationPolicyAllows(t *testing.T) {
	cases := []struct {
		name     string
		policy   RegistrationPolicy
		username string
		want     bool
	}{
		{"disabled denies everyone", RegistrationPolicy{Enabled: false}, "alice", false},
		{"disabled denies even listed users", RegistrationPolicy{Enabled: false, Allowlist: []string{"alice"}}, "alice", false},
		{"enabled with empty list allows anyone", RegistrationPolicy{Enabled: true}, "carol", true},
		{"enabled with list allows listed user", RegistrationPolicy{Enabled: true, Allowlist: []string{"alice"}}, "alice", true},
		{"enabled with list denies unlisted user", RegistrationPolicy{Enabled: true, Allowlist: []string{"alice"}}, "carol", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.Allows(tc.username); got != tc.want {
				t.Errorf("policy %+v Allows(%q) = %t, want %t", tc.policy, tc.username, got, tc.want)
			}
		})
	}
}

func TestRegistrationPolicyMode(t *testing.T) {
	cases := []struct {
		name   string
		policy RegistrationPolicy
		want   string
	}{
		{"disabled is closed", RegistrationPolicy{Enabled: false}, RegistrationModeClosed},
		{"disabled with allowlist is still closed", RegistrationPolicy{Enabled: false, Allowlist: []string{"alice"}}, RegistrationModeClosed},
		{"enabled without allowlist is open", RegistrationPolicy{Enabled: true}, RegistrationModeOpen},
		{"enabled with allowlist is invite only", RegistrationPolicy{Enabled: true, Allowlist: []string{"alice"}}, RegistrationModeInviteOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.Mode(); got != tc.want {
				t.Errorf("Mode() = %q, want %q", got, tc.want)
			}
		})
	}
}
