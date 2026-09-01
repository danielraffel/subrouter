//go:build windows

package accounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOpenPrivateStoreAuthorityKeyRejectsUntrustedAllowedAccess(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		mask windows.ACCESS_MASK
	}{
		{name: "read", mask: windows.GENERIC_READ},
		{name: "non-read nonzero", mask: windows.SYNCHRONIZE},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "authority-key")
			if err := os.WriteFile(path, make([]byte, 32), 0o600); err != nil {
				t.Fatal(err)
			}
			acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
				{
					AccessPermissions: windows.GENERIC_ALL,
					AccessMode:        windows.GRANT_ACCESS,
					Trustee: windows.TRUSTEE{
						TrusteeForm:  windows.TRUSTEE_IS_SID,
						TrusteeType:  windows.TRUSTEE_IS_USER,
						TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
					},
				},
				{
					AccessPermissions: test.mask,
					AccessMode:        windows.GRANT_ACCESS,
					Trustee: windows.TRUSTEE{
						TrusteeForm:  windows.TRUSTEE_IS_SID,
						TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
						TrusteeValue: windows.TrusteeValueFromSID(everyone),
					},
				},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := windows.SetNamedSecurityInfo(
				path,
				windows.SE_FILE_OBJECT,
				windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
				nil,
				nil,
				acl,
				nil,
			); err != nil {
				t.Fatal(err)
			}

			file, err := openPrivateStoreAuthorityKey(path)
			if file != nil {
				_ = file.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "grants access outside") {
				t.Fatalf("untrusted allow ACE was accepted: %v", err)
			}
		})
	}
}
