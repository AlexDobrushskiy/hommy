#!/usr/bin/env python3
"""Prints `unlocked` (exit 0) or `locked` (exit 1) for a keychain, without any chance of a dialog.

Usage: kc-status.py PATH | default. User interaction is disabled for this process first, so the Security framework
answers instead of asking (`security` has no such switch: on a locked keychain an item read raises the unlock dialog).
"""
import ctypes
import ctypes.util
import sys

sec = ctypes.cdll.LoadLibrary(ctypes.util.find_library('Security'))
sec.SecKeychainSetUserInteractionAllowed(ctypes.c_bool(False))
kc = ctypes.c_void_p()
arg = sys.argv[1] if len(sys.argv) > 1 else 'default'
if arg == 'default':
    err = sec.SecKeychainCopyDefault(ctypes.byref(kc))
else:
    err = sec.SecKeychainOpen(arg.encode(), ctypes.byref(kc))
if err:
    print(f'error {err}')
    sys.exit(2)
status = ctypes.c_uint32()
err = sec.SecKeychainGetStatus(kc, ctypes.byref(status))
if err:
    print(f'error {err}')
    sys.exit(2)
if status.value & 1:  # kSecUnlockStateStatus
    print('unlocked')
    sys.exit(0)
print('locked')
sys.exit(1)
