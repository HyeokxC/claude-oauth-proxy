//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#cgo CFLAGS: -Wno-deprecated-declarations
#include <Security/Security.h>
#include <stdlib.h>
#include <string.h>

// Locate an existing exact service/account item and retain its reference.
// Never add an item, including when lookup/update returns not-found.
static int update_existing(const char *service, unsigned int slen,
                           const char *account, unsigned int alen,
                           const void *expected, unsigned int elen,
                           const void *next, unsigned int nlen) {
    SecKeychainItemRef item = NULL;
    void *data = NULL;
    UInt32 length = 0;
    OSStatus status = SecKeychainFindGenericPassword(NULL, slen, service,
        alen, account, &length, &data, &item);
    if (status != errSecSuccess) return status == errSecItemNotFound ? 1 : 3;
    int matches = length == elen && memcmp(data, expected, elen) == 0;
    SecKeychainItemFreeContent(NULL, data);
    if (!matches) { CFRelease(item); return 2; }
    status = SecKeychainItemModifyAttributesAndData(item, NULL, nlen, next);
    CFRelease(item);
    return status == errSecSuccess ? 0 : (status == errSecItemNotFound ? 1 : 3);
}
*/
import "C"

import "unsafe"

func nativeKeychainUpdate(service, account string, expected, next []byte) error {
	s, a := C.CString(service), C.CString(account)
	defer C.free(unsafe.Pointer(s))
	defer C.free(unsafe.Pointer(a))
	e, n := C.CBytes(expected), C.CBytes(next)
	defer C.free(e)
	defer C.free(n)
	switch C.update_existing(s, C.uint(len(service)), a, C.uint(len(account)), e, C.uint(len(expected)), n, C.uint(len(next))) {
	case 0:
		return nil
	case 1:
		return errKeychainNotFound
	case 2:
		return errSourceChanged
	default:
		return errStoreWrite
	}
}
