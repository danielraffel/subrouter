//go:build windows

package accounts

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openPrivateStoreAuthorityKey(path string) (*os.File, error) {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	closeOnError := func(err error) (*os.File, error) {
		_ = file.Close()
		return nil, err
	}
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
		return closeOnError(err)
	}
	if opened.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return closeOnError(fmt.Errorf("account store authority key must be a private regular file"))
	}
	descriptor, err := windows.GetSecurityInfo(
		handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return closeOnError(fmt.Errorf("inspect account store authority ACL: %w", err))
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return closeOnError(fmt.Errorf("inspect account store authority owner: %w", err))
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return closeOnError(fmt.Errorf("account store authority key must be owned by the current user"))
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return closeOnError(fmt.Errorf("account store authority key must have a protected access list"))
	}
	trustedAccessSIDs := []*windows.SID{user.User.Sid}
	for _, sidType := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinBuiltinAdministratorsSid,
		windows.WinLocalSystemSid,
	} {
		sid, sidErr := windows.CreateWellKnownSid(sidType)
		if sidErr != nil {
			return closeOnError(fmt.Errorf("inspect account store authority ACL: %w", sidErr))
		}
		trustedAccessSIDs = append(trustedAccessSIDs, sid)
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return closeOnError(fmt.Errorf("inspect account store authority ACL entry: %w", err))
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask == 0 {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		trusted := false
		for _, trustedSID := range trustedAccessSIDs {
			if aceSID.Equals(trustedSID) {
				trusted = true
				break
			}
		}
		if !trusted {
			return closeOnError(fmt.Errorf("account store authority key grants access outside the current user"))
		}
	}
	return file, nil
}
