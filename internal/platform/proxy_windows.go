package platform

import (
	"syscall"
	"unsafe"
)

// SystemProxy returns the static proxy configured in Windows' Internet
// Options (per user).
func SystemProxy() Proxy {
	key, _ := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\Internet Settings`)
	var h syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, key, 0, syscall.KEY_READ, &h) != nil {
		return Proxy{}
	}
	defer syscall.RegCloseKey(h)
	if regDWORD(h, "ProxyEnable") == 0 {
		return Proxy{}
	}
	return parseWinProxy(regString(h, "ProxyServer"), regString(h, "ProxyOverride"))
}

func regDWORD(h syscall.Handle, name string) uint32 {
	p, _ := syscall.UTF16PtrFromString(name)
	var v, typ uint32
	n := uint32(4)
	if syscall.RegQueryValueEx(h, p, nil, &typ, (*byte)(unsafe.Pointer(&v)), &n) != nil || typ != syscall.REG_DWORD {
		return 0
	}
	return v
}

func regString(h syscall.Handle, name string) string {
	p, _ := syscall.UTF16PtrFromString(name)
	var typ, n uint32
	if syscall.RegQueryValueEx(h, p, nil, &typ, nil, &n) != nil || n < 2 || (typ != syscall.REG_SZ && typ != syscall.REG_EXPAND_SZ) {
		return ""
	}
	buf := make([]uint16, n/2+1)
	if syscall.RegQueryValueEx(h, p, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n) != nil {
		return ""
	}
	return syscall.UTF16ToString(buf)
}
