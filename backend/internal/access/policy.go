package access

const (
	RegistrationModeOpen       = "open"
	RegistrationModeInviteOnly = "invite_only"
	RegistrationModeClosed     = "closed"
)

type RegistrationPolicy struct {
	Enabled   bool
	Allowlist []string
}

func Allows(list []string, username string) bool {
	if len(list) == 0 {
		return true
	}
	for _, entry := range list {
		if entry == username {
			return true
		}
	}
	return false
}

func (p RegistrationPolicy) Allows(username string) bool {
	return p.Enabled && Allows(p.Allowlist, username)
}

func (p RegistrationPolicy) Mode() string {
	if !p.Enabled {
		return RegistrationModeClosed
	}
	if len(p.Allowlist) == 0 {
		return RegistrationModeOpen
	}
	return RegistrationModeInviteOnly
}
