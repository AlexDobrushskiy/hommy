// Package vault writes the embedded vault template (master spec §6): `hommy
// vault init` into an empty folder, `hommy vault add-recipes` into an existing
// vault without touching a file that is already there.
package vault

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	vaulttemplate "github.com/AlexDobrushskiy/hommy/vault-template"
)

// ErrNotEmpty: init refuses a folder that holds anything (exit 3).
var ErrNotEmpty = errors.New("not empty")

// Files lists the template's files (slash paths), sorted; Go sources are not part of it.
func Files() []string {
	var out []string
	fs.WalkDir(vaulttemplate.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasSuffix(p, ".go") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// Init writes the whole template into dir, which must be absent or empty.
func Init(dir string) (int, error) {
	if ents, err := os.ReadDir(dir); err == nil {
		if len(ents) > 0 {
			return 0, fmt.Errorf("%s is %w", dir, ErrNotEmpty)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		if fi, serr := os.Stat(dir); serr == nil && !fi.IsDir() {
			return 0, fmt.Errorf("%s is a file, not a folder", dir)
		}
		return 0, err
	}
	added, err := writeMissing(dir)
	return len(added), err
}

// AddRecipes copies every template file dir lacks (the recipes in prompt/ and
// any missing kernel or context file) and never overwrites one. It returns the
// files it added and the routing-table rows that send an errand to a recipe,
// for the user to add to their own kernel.
func AddRecipes(dir string) (added []string, rows []string, err error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, nil, err
	}
	if !fi.IsDir() {
		return nil, nil, fmt.Errorf("%s is a file, not a folder", dir)
	}
	if added, err = writeMissing(dir); err != nil {
		return added, nil, err
	}
	return added, RecipeRows(), nil
}

func writeMissing(dir string) ([]string, error) {
	var added []string
	for _, p := range Files() {
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if _, err := os.Lstat(dst); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return added, err
		}
		b, err := fs.ReadFile(vaulttemplate.FS, p)
		if err != nil {
			return added, err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return added, err
		}
		// O_EXCL: a file that appeared meanwhile is never overwritten
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return added, err
		}
		if _, err := f.Write(b); err != nil {
			f.Close()
			return added, err
		}
		if err := f.Close(); err != nil {
			return added, err
		}
		added = append(added, p)
	}
	return added, nil
}

// RecipeRows are the template kernel's routing rows that name a recipe in prompt/.
func RecipeRows() []string {
	b, err := fs.ReadFile(vaulttemplate.FS, "AGENTS.md")
	if err != nil {
		return nil
	}
	var rows []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "|") && strings.Contains(l, "prompt/") {
			rows = append(rows, l)
		}
	}
	return rows
}
