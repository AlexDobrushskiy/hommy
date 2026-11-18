package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/config"
	"github.com/AlexDobrushskiy/hommy/internal/secret"
)

// signer owns the instance's signing keychain and identity. Every security /
// codesign call has a timeout, stdin closed (or a command script on stdin) and
// HOME = the instance's home, so no call can raise a dialog or reach another
// user's keychain setup. Passwords travel only over `security -i` stdin.
type signer struct {
	inst config.Instance
	out  io.Writer
}

func (m *Manager) signer() signer { return signer{inst: m.inst(), out: m.Out} }

// IdentityName is the signing certificate's CN for an instance.
func IdentityName(inst config.Instance) string { return inst.Base() + " local signing" }

func (s signer) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "HOME=") && !strings.HasPrefix(kv, "CFFIXED_USER_HOME=") {
			env = append(env, kv)
		}
	}
	return append(env, "HOME="+s.inst.Home)
}

func (s signer) store() secret.Store {
	return secret.Store{Service: s.inst.KeychainService(), Env: s.env()}
}

func (s signer) kc() string { return s.inst.SigningKeychain() }

// run runs a tool; stdin "" = closed.
func (s signer) run(timeout time.Duration, stdin, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = s.env()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	name := filepath.Base(bin)
	if len(args) > 0 {
		name += " " + args[0]
	}
	if ctx.Err() != nil {
		return out.String(), fmt.Errorf("%s: timed out", name)
	}
	if err != nil {
		return out.String(), fmt.Errorf("%s: %s", name, firstLine(out.String(), err))
	}
	return out.String(), nil
}

func (s signer) security(args ...string) (string, error) {
	return s.run(20*time.Second, "", "/usr/bin/security", args...)
}

// securityStdin runs one security command that carries a password, through
// `security -i` so the password never reaches argv. The error never echoes it.
func (s signer) securityStdin(line string) error {
	out, err := s.run(20*time.Second, line+"\n", "/usr/bin/security", "-i")
	cmd, _, _ := strings.Cut(line, " ")
	if err != nil || strings.Contains(out, ": returned ") {
		return fmt.Errorf("security %s failed on %s", cmd, s.kc())
	}
	return nil
}

func q(p string) string { return `"` + strings.ReplaceAll(p, `"`, `\"`) + `"` }

func (s signer) unlock(pw string) error {
	if err := s.securityStdin(fmt.Sprintf("unlock-keychain -p %s %s", pw, q(s.kc()))); err != nil {
		return err
	}
	_, err := s.security("set-keychain-settings", s.kc()) // no auto-lock, no lock on sleep
	return err
}

// ensureIdentity makes sure the signing keychain exists, is unlocked and holds
// the code-signing identity; it returns the identity's SHA-1.
func (s signer) ensureIdentity() (string, error) {
	st := s.store()
	pw, err := st.Get(secret.SigningPassword)
	if err != nil && !errors.Is(err, secret.ErrNotFound) {
		return "", err
	}
	if exists(s.kc()) {
		if pw == "" || s.unlock(pw) != nil {
			// a keychain we cannot open holds nothing but a regenerable identity
			if err := os.Remove(s.kc()); err != nil {
				return "", err
			}
			fmt.Fprintf(s.outw(), "recreating %s (it cannot be unlocked)\n", s.kc())
		}
	}
	if !exists(s.kc()) {
		if pw, err = secret.NewKey(); err != nil {
			return "", err
		}
		if err := st.Set(secret.SigningPassword, pw); err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(s.kc()), 0o700); err != nil {
			return "", err
		}
		if err := s.securityStdin(fmt.Sprintf("create-keychain -p %s %s", pw, q(s.kc()))); err != nil {
			return "", err
		}
		if err := s.unlock(pw); err != nil {
			return "", err
		}
	}
	if h, err := s.findIdentity(); err != nil || h != "" {
		return h, err
	}
	if err := s.createIdentity(pw); err != nil {
		return "", err
	}
	h, err := s.findIdentity()
	if err == nil && h == "" {
		err = fmt.Errorf("signing identity %q missing after import into %s", IdentityName(s.inst), s.kc())
	}
	return h, err
}

func (s signer) outw() io.Writer {
	if s.out == nil {
		return io.Discard
	}
	return s.out
}

var identityLine = regexp.MustCompile(`\)\s+([0-9A-F]{40})\s+"([^"]*)"`)

// findIdentity returns the SHA-1 of the instance's code-signing identity in its keychain ("" = none).
func (s signer) findIdentity() (string, error) {
	out, err := s.security("find-identity", "-p", "codesigning", s.kc())
	if err != nil {
		return "", err
	}
	for _, m := range identityLine.FindAllStringSubmatch(out, -1) {
		if m[2] == IdentityName(s.inst) {
			return m[1], nil
		}
	}
	return "", nil
}

// createIdentity makes a self-signed code-signing certificate (EKU codeSigning)
// and imports it with its key; codesign may use the key without a dialog.
func (s signer) createIdentity(pw string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: IdentityName(s.inst)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(30, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "hommy-sign-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	keyFile, certFile := filepath.Join(dir, "key.pem"), filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return err
	}
	if _, err := s.security("import", keyFile, "-k", s.kc(), "-t", "priv", "-f", "openssl",
		"-T", "/usr/bin/codesign"); err != nil {
		return err
	}
	if _, err := s.security("import", certFile, "-k", s.kc(), "-t", "cert", "-f", "pemseq"); err != nil {
		return err
	}
	return s.securityStdin(fmt.Sprintf("set-key-partition-list -S apple-tool:,apple: -s -k %s %s", pw, q(s.kc())))
}

// searchList is the user keychain search list (HOME = the instance's).
func (s signer) searchList() ([]string, error) {
	out, err := s.security("list-keychains", "-d", "user")
	if err != nil {
		return nil, err
	}
	var l []string
	for _, line := range strings.Split(out, "\n") {
		if k := strings.Trim(strings.TrimSpace(line), `"`); k != "" {
			l = append(l, k)
		}
	}
	return l, nil
}

// withSearchList runs fn with the signing keychain on the user search list
// (codesign finds identities only there) and restores the list after, also on error.
func (s signer) withSearchList(fn func() error) error {
	orig, err := s.searchList()
	if err != nil {
		return err
	}
	for _, k := range orig {
		if k == s.kc() {
			return fn()
		}
	}
	if _, err := s.security(append([]string{"list-keychains", "-d", "user", "-s"}, append(orig, s.kc())...)...); err != nil {
		return err
	}
	ferr := fn()
	_, rerr := s.security(append([]string{"list-keychains", "-d", "user", "-s"}, orig...)...)
	if rerr != nil {
		rerr = fmt.Errorf("restoring the keychain search list: %w", rerr)
	}
	return errors.Join(ferr, rerr)
}

// sign signs path with the instance identity and identifier dev.hommy, then
// checks the designated requirement (Requirement).
func (s signer) sign(path string) error {
	hash, err := s.ensureIdentity()
	if err != nil {
		return fmt.Errorf("signing identity: %w", err)
	}
	err = s.withSearchList(func() error {
		_, err := s.run(60*time.Second, "", "/usr/bin/codesign", "--force", "--sign", hash,
			"--identifier", config.SigningIdentifier, "--keychain", s.kc(), path)
		return err
	})
	if err != nil {
		return err
	}
	_, err = Requirement(path)
	return err
}

// deleteKeychain removes the signing keychain (delete-keychain when it can be
// unlocked, which also drops it from the search list; else the file).
func (s signer) deleteKeychain() error {
	if pw, err := s.store().Get(secret.SigningPassword); err == nil && s.unlock(pw) == nil {
		if _, err := s.security("delete-keychain", s.kc()); err == nil {
			return nil
		}
	}
	if err := os.Remove(s.kc()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Requirement returns path's designated requirement and fails unless the
// signature is valid, names identifier dev.hommy and a certificate leaf, and
// pins no cdhash (a cdhash requirement loses TCC grants on every upgrade).
func Requirement(path string) (string, error) {
	run := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/usr/bin/codesign", args...).CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("codesign %s %s: %s", args[0], path, firstLine(string(out), err))
		}
		return string(out), nil
	}
	if _, err := run("--verify", "--strict", path); err != nil {
		return "", err
	}
	out, err := run("-d", "-r-", path)
	if err != nil {
		return "", err
	}
	req := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "designated =>") {
			req = strings.TrimSpace(strings.TrimPrefix(line, "designated =>"))
		}
	}
	if req == "" || strings.Contains(req, "cdhash") || !strings.Contains(req, `identifier "`+config.SigningIdentifier+`"`) ||
		!strings.Contains(req, "certificate leaf") {
		return req, fmt.Errorf("%s: designated requirement %q is not `identifier \"%s\" and certificate leaf = H\"…\"`",
			path, req, config.SigningIdentifier)
	}
	return req, nil
}
