package workspace

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Access to private repositories and images (docs/todo.md private_images): a project keeps per host one user name and token (GitHub: a classic PAT with repo and read:packages, GitLab: a deploy token with read_repository and read_registry, Forgejo: a token with read:repository and read:package). The token is stored encrypted and never handed out again. It is used where the host comes up: the check of a finished image (registry), the check and the checkout of the material (Git), and on the nodes as the project's pull secret (ws-pull-<slug>) and the checkout's Git credentials (ws-git-<slug>), both kept by the GPU part in the workspace namespace (SetCredentialSecrets). Builds from private repositories are not offered: their images would land in the public Harbor project envs.

// PullSecretName and GitSecretName are a project's secrets in the workspace namespace.
func PullSecretName(slug string) string { return "ws-pull-" + slug }
func GitSecretName(slug string) string  { return "ws-git-" + slug }

// Credential is a project's access to one host.
type Credential struct {
	ID        uint   `gorm:"primaryKey"`
	Project   string `gorm:"uniqueIndex:idx_cred_project_host;not null"`
	Host      string `gorm:"uniqueIndex:idx_cred_project_host;not null"`
	Username  string `gorm:"not null"`
	Sealed    []byte `gorm:"not null"` // the token, AES-GCM with the service's key
	CreatedBy string
	UpdatedAt time.Time
}

func (Credential) TableName() string { return "workspace_credentials" }

// CredentialInfo is a credential as it is shown: without its token.
type CredentialInfo struct {
	Host      string    `json:"host" example:"gitlab.dhbw.de"`
	Username  string    `json:"username" example:"gitlab+deploy-token-12"`
	CreatedBy string    `json:"created_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Basic is a user name and token for a host.
type Basic struct {
	Username string
	Token    string
}

// ProjectSecrets is what the nodes get of a project's credentials: a Docker config for the pull secret and git's credential store for the checkout.
type ProjectSecrets struct {
	DockerConfig   []byte
	GitCredentials []byte
}

// CredentialStore keeps the credentials.
type CredentialStore interface {
	List(ctx context.Context, project string) ([]Credential, error)
	All(ctx context.Context) ([]Credential, error)
	Put(ctx context.Context, c Credential) error
	Delete(ctx context.Context, project, host string) error
}

// SetCredentials enables credentials: their store, the key that seals the tokens (32 bytes) and where the projects' secrets go. Without it credentials are refused.
func (s *Service) SetCredentials(store CredentialStore, key []byte, secrets func(ctx context.Context, want map[string]ProjectSecrets) error) error {
	if len(key) != 32 {
		return fmt.Errorf("credentials key: %d bytes, want 32", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	s.creds, s.aead, s.credSecrets = store, aead, secrets
	return nil
}

var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

// normaliseHost is the host as credentials are keyed: lower case, without scheme and path; Docker Hub under one name.
func normaliseHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if u, err := url.Parse(h); err == nil && u.Host != "" {
		h = u.Host
	}
	h = strings.TrimSuffix(strings.SplitN(h, "/", 2)[0], ".")
	switch h {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		h = "docker.io"
	}
	return h
}

// Credentials lists a project's credentials without their tokens.
func (s *Service) Credentials(ctx context.Context, project string) ([]CredentialInfo, error) {
	if s.creds == nil {
		return []CredentialInfo{}, nil
	}
	list, err := s.creds.List(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]CredentialInfo, 0, len(list))
	for _, c := range list {
		out = append(out, CredentialInfo{Host: c.Host, Username: c.Username, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// SetCredential stores (or replaces) a project's access to a host and brings the projects' secrets up to date.
func (s *Service) SetCredential(ctx context.Context, project, host, username, token, by string) error {
	if s.creds == nil {
		return refuse("Zugänge sind hier nicht eingerichtet.")
	}
	if _, err := s.store.Get(ctx, project); err != nil {
		return err
	}
	host, username, token = normaliseHost(host), strings.TrimSpace(username), strings.TrimSpace(token)
	if !hostRE.MatchString(host) || !strings.Contains(host, ".") {
		return refuse("Host %q ungültig: nur der Name, z. B. gitlab.dhbw.de oder ghcr.io.", host)
	}
	if username == "" || token == "" || strings.ContainsAny(username, ":\n") || strings.ContainsAny(token, "\n ") {
		return refuse("Benutzername und Token werden gebraucht (Benutzername ohne Doppelpunkt, Token ohne Leerzeichen).")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(token), []byte(project+"\x00"+host))
	if err := s.creds.Put(ctx, Credential{Project: project, Host: host, Username: username, Sealed: sealed, CreatedBy: by, UpdatedAt: s.now()}); err != nil {
		return err
	}
	s.materialOK.Range(func(k, _ any) bool { s.materialOK.Delete(k); return true })
	return s.SyncCredentialSecrets(ctx)
}

// DeleteCredential removes a project's access to a host.
func (s *Service) DeleteCredential(ctx context.Context, project, host string) error {
	if s.creds == nil {
		return ErrNotFound
	}
	if err := s.creds.Delete(ctx, project, normaliseHost(host)); err != nil {
		return err
	}
	return s.SyncCredentialSecrets(ctx)
}

// credentialFor is a project's user and token for a host; nil = none (or the token cannot be opened).
func (s *Service) credentialFor(ctx context.Context, project, host string) *Basic {
	if s.creds == nil || project == "" {
		return nil
	}
	list, err := s.creds.List(ctx, project)
	if err != nil {
		return nil
	}
	host = normaliseHost(host)
	for _, c := range list {
		if c.Host == host {
			if token, err := s.open(c); err == nil {
				return &Basic{Username: c.Username, Token: token}
			}
		}
	}
	return nil
}

func (s *Service) open(c Credential) (string, error) {
	n := s.aead.NonceSize()
	if len(c.Sealed) < n {
		return "", errors.New("sealed token too short")
	}
	plain, err := s.aead.Open(nil, c.Sealed[:n], c.Sealed[n:], []byte(c.Project+"\x00"+c.Host))
	return string(plain), err
}

// hostOfGit is the host of a Git address.
func hostOfGit(gitURL string) string {
	u, err := url.Parse(strings.TrimSpace(gitURL))
	if err != nil {
		return ""
	}
	return normaliseHost(u.Host)
}

// HasCredentials tells whether a project has any credentials, i.e. whether its pods get its secrets.
func (s *Service) HasCredentials(ctx context.Context, project string) bool {
	if s.creds == nil {
		return false
	}
	list, err := s.creds.List(ctx, project)
	return err == nil && len(list) > 0
}

// SyncCredentialSecrets hands the secrets of every project with credentials to the GPU part, which creates, changes and removes them.
func (s *Service) SyncCredentialSecrets(ctx context.Context) error {
	if s.creds == nil || s.credSecrets == nil {
		return nil
	}
	all, err := s.creds.All(ctx)
	if err != nil {
		return err
	}
	byProject := map[string][]Credential{}
	for _, c := range all {
		byProject[c.Project] = append(byProject[c.Project], c)
	}
	want := map[string]ProjectSecrets{}
	for project, list := range byProject {
		auths := map[string]any{}
		var git []string
		for _, c := range list {
			token, err := s.open(c)
			if err != nil {
				continue
			}
			pair := c.Username + ":" + token
			registry := c.Host
			if registry == "docker.io" {
				registry = "https://index.docker.io/v1/"
			}
			auths[registry] = map[string]any{"auth": base64.StdEncoding.EncodeToString([]byte(pair))}
			git = append(git, "https://"+url.QueryEscape(c.Username)+":"+url.QueryEscape(token)+"@"+c.Host)
		}
		if len(git) == 0 {
			continue
		}
		slices.Sort(git)
		docker, _ := json.Marshal(map[string]any{"auths": auths})
		want[project] = ProjectSecrets{DockerConfig: docker, GitCredentials: []byte(strings.Join(git, "\n") + "\n")}
	}
	return s.credSecrets(ctx, want)
}

// MemoryCredentials keeps credentials in memory (tests, development).
type MemoryCredentials struct {
	mu sync.Mutex
	m  map[string]Credential // project + "\x00" + host
}

func NewMemoryCredentials() *MemoryCredentials { return &MemoryCredentials{m: map[string]Credential{}} }

func (m *MemoryCredentials) List(_ context.Context, project string) ([]Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Credential
	for _, c := range m.m {
		if c.Project == project {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *MemoryCredentials) All(context.Context) ([]Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Credential, 0, len(m.m))
	for _, c := range m.m {
		out = append(out, c)
	}
	return out, nil
}

func (m *MemoryCredentials) Put(_ context.Context, c Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[c.Project+"\x00"+c.Host] = c
	return nil
}

func (m *MemoryCredentials) Delete(_ context.Context, project, host string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.m[project+"\x00"+host]; !ok {
		return ErrNotFound
	}
	delete(m.m, project+"\x00"+host)
	return nil
}

// GormCredentials keeps credentials in the database.
type GormCredentials struct{ db *gorm.DB }

func NewGormCredentials(db *gorm.DB) (*GormCredentials, error) {
	if err := db.AutoMigrate(&Credential{}); err != nil {
		return nil, err
	}
	return &GormCredentials{db: db}, nil
}

func (g *GormCredentials) List(ctx context.Context, project string) ([]Credential, error) {
	var out []Credential
	return out, g.db.WithContext(ctx).Where("project = ?", project).Find(&out).Error
}

func (g *GormCredentials) All(ctx context.Context) ([]Credential, error) {
	var out []Credential
	return out, g.db.WithContext(ctx).Find(&out).Error
}

func (g *GormCredentials) Put(ctx context.Context, c Credential) error {
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("project = ? AND host = ?", c.Project, c.Host).Delete(&Credential{}).Error; err != nil {
			return err
		}
		return tx.Create(&c).Error
	})
}

func (g *GormCredentials) Delete(ctx context.Context, project, host string) error {
	res := g.db.WithContext(ctx).Where("project = ? AND host = ?", project, host).Delete(&Credential{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
