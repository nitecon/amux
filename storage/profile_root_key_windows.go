//go:build windows

package storage

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var profileRootKeyPrefix = []byte("AMUX-DPAPI-1\x00")

func protectProfileRootKey(key []byte) ([]byte, error) {
	input := windows.DataBlob{Size: uint32(len(key)), Data: &key[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
	wrapped := append([]byte(nil), profileRootKeyPrefix...)
	return append(wrapped, unsafe.Slice(output.Data, output.Size)...), nil
}

func unprotectProfileRootKey(key []byte) ([]byte, error) {
	if len(key) < len(profileRootKeyPrefix) || string(key[:len(profileRootKeyPrefix)]) != string(profileRootKeyPrefix) {
		return key, nil
	}
	data := key[len(profileRootKeyPrefix):]
	if len(data) == 0 {
		return nil, fmt.Errorf("protected root key is empty")
	}
	input := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
	return append([]byte(nil), unsafe.Slice(output.Data, output.Size)...), nil
}
