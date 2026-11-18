// Package config is hommy's instance, its paths and its one config file
// (<config dir>/hommy.env): KEY=VALUE lines, process env > file > default.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var instanceRe = regexp.MustCompile(`^[a-z0-9-]{1,24}$`)

// Instance names one install on a Mac; "" is the default instance.
type Instance struct {
	Name string
	Home string // the user's HOME every path hangs off
}

// NewInstance validates name ("" = default) and binds it to home.
func NewInstance(name, home string) (Instance, error) {
	if name != "" && !instanceRe.MatchString(name) {
		return Instance{}, fmt.Errorf("instance %q: want [a-z0-9-]{1,24}", name)
	}
	if home == "" {
		return Instance{}, fmt.Errorf("HOME is not set")
	}
	return Instance{Name: name, Home: home}, nil
}

// FromEnv is the instance named by flag (else HOMMY_INSTANCE) under $HOME.
func FromEnv(flagName string) (Instance, error) {
	name := flagName
	if name == "" {
		name = os.Getenv("HOMMY_INSTANCE")
	}
	home, _ := os.UserHomeDir()
	return NewInstance(name, home)
}

func (i Instance) suffix() string {
	if i.Name == "" {
		return ""
	}
	return "-" + i.Name
}

// Base is "hommy" or "hommy-X": dir names and the Keychain service.
func (i Instance) Base() string { return "hommy" + i.suffix() }

func (i Instance) ConfigDir() string { return filepath.Join(i.Home, ".config", i.Base()) }
func (i Instance) StateDir() string  { return filepath.Join(i.Home, ".local", "state", i.Base()) }
func (i Instance) ShareDir() string  { return filepath.Join(i.Home, ".local", "share", i.Base()) }

// ConfigFile is the default config file path.
func (i Instance) ConfigFile() string { return filepath.Join(i.ConfigDir(), "hommy.env") }

// BinPath is the signed stable copy launchd runs.
func (i Instance) BinPath() string { return filepath.Join(i.ShareDir(), "bin", "hommy") }

// KeychainService is the generic-password service of the instance's secrets.
func (i Instance) KeychainService() string { return i.Base() }

// SigningKeychain is the instance's own code-signing keychain.
func (i Instance) SigningKeychain() string {
	return filepath.Join(i.Home, "Library", "Keychains", i.Base()+"-signing.keychain-db")
}

// LabelPrefix is "dev.hommy" or "dev.hommy.X".
func (i Instance) LabelPrefix() string {
	if i.Name == "" {
		return "dev.hommy"
	}
	return "dev.hommy." + i.Name
}

// Label is the launchd label of a service (hub, host, speech).
func (i Instance) Label(svc string) string { return i.LabelPrefix() + "." + svc }

// PlistPath is where the service's launchd plist lives.
func (i Instance) PlistPath(svc string) string {
	return filepath.Join(i.Home, "Library", "LaunchAgents", i.Label(svc)+".plist")
}

// LogDir is <state>/<svc>, holding out.log and err.log.
func (i Instance) LogDir(svc string) string { return filepath.Join(i.StateDir(), svc) }

// SigningIdentifier is the codesign identifier of the stable copy.
const SigningIdentifier = "dev.hommy"

// ExpandHome turns ~ and ~/x into paths under home.
func ExpandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
