package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Material is the JAMF material: profile template, package directory, the
// fleet README.
type Material struct {
	ProfileTemplate string // path of de.dhbw.llm.mobileconfig.tmpl
	PackageDir      string // where build-bundle.sh --upload puts the .pkg
	ReadmePath      string // README-FLOTTE.md
}

// PackageInfo describes the newest uploaded package.
type PackageInfo struct {
	File       string    `json:"file"`
	Version    string    `json:"version"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploaded_at"`
	SHA256     string    `json:"sha256"`
}

var pkgRE = regexp.MustCompile(`^DHBW-LLM-Fleet-[\w.-]+\.pkg$`)

var ErrNoPackage = errors.New("no package uploaded")

// Package returns the newest package, or nil.
func (m Material) Package() *PackageInfo {
	entries, err := os.ReadDir(m.PackageDir)
	if err != nil {
		return nil
	}
	type cand struct {
		name string
		info os.FileInfo
	}
	var cs []cand
	for _, e := range entries {
		if !pkgRE.MatchString(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil {
			cs = append(cs, cand{e.Name(), fi})
		}
	}
	if len(cs) == 0 {
		return nil
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].info.ModTime().After(cs[j].info.ModTime()) })
	c := cs[0]
	sum := ""
	if b, err := os.ReadFile(filepath.Join(m.PackageDir, c.name+".sha256")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			sum = f[0]
		}
	}
	return &PackageInfo{File: c.name, Size: c.info.Size(), UploadedAt: c.info.ModTime().UTC(), SHA256: sum,
		Version: strings.TrimSuffix(strings.TrimPrefix(c.name, "DHBW-LLM-Fleet-"), ".pkg")}
}

// PackagePath is the file of the newest package.
func (m Material) PackagePath() (string, string, error) {
	p := m.Package()
	if p == nil {
		return "", "", ErrNoPackage
	}
	return filepath.Join(m.PackageDir, p.File), p.File, nil
}

// DeletePackages removes all uploaded packages and their checksums. The
// machines are unaffected; only the download disappears.
func (m Material) DeletePackages() error {
	entries, err := os.ReadDir(m.PackageDir)
	if err != nil {
		return nil
	}
	sumRE := regexp.MustCompile(`^DHBW-LLM-Fleet-[\w.-]+\.pkg(\.sha256)?$`)
	for _, e := range entries {
		if sumRE.MatchString(e.Name()) {
			if err := os.Remove(filepath.Join(m.PackageDir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// ProfileOptions are optional per device group.
type ProfileOptions struct {
	Location, Operator, Contact string
}

func xmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// stableUUID derives a UUID from the content: downloading the same profile
// twice replaces it in JAMF instead of adding a second one.
func stableUUID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	x := hex.EncodeToString(h[:])
	return strings.ToUpper(x[0:8] + "-" + x[8:12] + "-4" + x[13:16] + "-a" + x[17:20] + "-" + x[20:32])
}

var optionalKeysRE = regexp.MustCompile(`(?m)^[ \t]*<!-- __OPTIONAL_KEYS__ -->\n`)

// Mobileconfig renders the JAMF configuration profile.
func (m Material) Mobileconfig(cfg Config, o ProfileOptions) ([]byte, error) {
	tmpl, err := os.ReadFile(m.ProfileTemplate)
	if err != nil {
		return nil, err
	}
	url := "https://" + cfg.EnrollHostname
	var extra strings.Builder
	for _, kv := range [][2]string{{"Location", o.Location}, {"Operator", o.Operator}, {"Contact", o.Contact}} {
		if kv[1] != "" {
			extra.WriteString("            <key>" + kv[0] + "</key>\n            <string>" + xmlEsc(kv[1]) + "</string>\n")
		}
	}
	out := string(tmpl)
	out = strings.Replace(out, "__ENROLL_URL__", xmlEsc(url), 1)
	out = strings.Replace(out, "__ENROLL_TOKEN__", xmlEsc(cfg.EnrollToken), 1)
	out = strings.Replace(out, "__UUID_TOP__", stableUUID("top", url, cfg.EnrollToken, o.Location, o.Operator, o.Contact), 1)
	out = strings.Replace(out, "__UUID_INNER__", stableUUID("inner", url, cfg.EnrollToken, o.Location, o.Operator, o.Contact), 1)
	loc := optionalKeysRE.FindStringIndex(out)
	if loc != nil {
		out = out[:loc[0]] + extra.String() + out[loc[1]:]
	}
	return []byte(out), nil
}
