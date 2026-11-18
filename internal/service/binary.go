package service

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// stampPath records the SHA-256 of the source binary the signed copy was made
// from (signing rewrites the copy, so its own hash never equals the source's).
func (m *Manager) stampPath() string {
	return filepath.Join(filepath.Dir(m.inst().BinPath()), ".hommy.sha256")
}

func (m *Manager) exe() (string, error) {
	p := m.Exe
	if p == "" {
		var err error
		if p, err = os.Executable(); err != nil {
			return "", err
		}
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Abs(p)
}

func fileSHA(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (m *Manager) isSigned(p string) bool {
	if m.signedOK != nil {
		return m.signedOK(p)
	}
	_, err := Requirement(p)
	return err == nil
}

func (m *Manager) signFile(p string) error {
	if m.sign != nil {
		return m.sign(p)
	}
	return m.signer().sign(p)
}

// ensureBinary keeps BinPath a signed copy of the running binary: copied only
// when the source's SHA-256 changed, signed only when copied or when the
// signature is missing or invalid. The copy is signed before it replaces the
// old one (atomic rename), so BinPath is always a signed binary.
func (m *Manager) ensureBinary() (changed bool, err error) {
	dst := m.inst().BinPath()
	src, err := m.exe()
	if err != nil {
		return false, err
	}
	self := false
	if r, err := filepath.EvalSymlinks(dst); err == nil && r == src {
		self = true
	}
	sum := ""
	if !self {
		if sum, err = fileSHA(src); err != nil {
			return false, err
		}
	}
	stamp, _ := os.ReadFile(m.stampPath())
	fresh := exists(dst) && (self || strings.TrimSpace(string(stamp)) == sum)
	if fresh && m.isSigned(dst) {
		m.printf("unchanged binary")
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	from := src
	if self {
		from = dst
	}
	tmp, err := copyTemp(from, filepath.Dir(dst))
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp)
	if err := m.signFile(tmp); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return false, err
	}
	if !self {
		if err := os.WriteFile(m.stampPath(), []byte(sum+"\n"), 0o644); err != nil {
			return false, err
		}
	}
	m.Changes++
	m.printf("signed %s", dst)
	return true, nil
}

// copyTemp copies src to a new 0755 temp file in dir.
func copyTemp(src, dir string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(dir, ".hommy-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(out.Name())
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(out.Name())
		return "", err
	}
	if err := os.Chmod(out.Name(), 0o755); err != nil {
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}
