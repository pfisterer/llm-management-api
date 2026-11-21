// Package modeldesc keeps the descriptions of the served models: per model name (as LiteLLM's deployments serve it: a GPU catalogue model like Qwen/Qwen3.6-35B-A3B-FP8 or a Mac fleet model like qwen3.6:35b-a3b-coding-nvfp4) a text, its origin, licence and a few facts, edited by admins and shown to everybody who can use the model, so the models behind the names are transparent.
package modeldesc

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pfisterer/cloud-self-service-golib/msg"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Description is what is said about one served model.
type Description struct {
	// Model is the served model name, as the deployments report it (litellm_params.model without openai/).
	Model string `json:"model" gorm:"primaryKey" example:"qwen3.6:35b-a3b-coding-nvfp4"`
	// Summary is one short sentence for lists; empty = the first paragraph of Description.
	Summary string `json:"summary" example:"Qwen3.6 35B-A3B for code, 4-bit for Apple silicon."`
	// Description is Markdown; clients render it sanitised.
	Description string `json:"description" example:"Mixture-of-experts model with 35B parameters, 3B active per token."`
	// SourceURL is where the model comes from, e.g. its Hugging Face page (http or https only).
	SourceURL string `json:"source_url" example:"https://huggingface.co/Qwen/Qwen3.6-35B-A3B"`
	License   string `json:"license" example:"Apache-2.0"`
	// Parameters as said by the maker, e.g. "35B (3B active)".
	Parameters string `json:"parameters" example:"35B (3B active)"`
	// Quantisation, e.g. "FP8", "NVFP4", "IQ3_S"; empty = none or unknown.
	Quantisation string `json:"quantisation" example:"NVFP4"`
	// ToolCalls: the model calls tools (function calling) here; null = unknown.
	ToolCalls *bool `json:"tool_calls" extensions:"x-nullable"`
	// Reasoning: the model thinks before it answers (reasoning mode); null = unknown.
	Reasoning *bool     `json:"reasoning" extensions:"x-nullable"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by"`
}

func (Description) TableName() string { return "model_descriptions" }

// Limits of the fields, in characters.
const (
	MaxModelLen       = 200
	MaxSummaryLen     = 300
	MaxDescriptionLen = 20000
	MaxShortLen       = 200
	MaxURLLen         = 500
)

// Invalid is a description refused for its content, with a text for people.
type Invalid struct{ Reason msg.Text }

func (e *Invalid) Error() string { return e.Reason.In(msg.EN) }

// Normalize trims the fields and checks them: a model name, a source URL that is http(s) (a link clients put in an href), and the lengths.
func Normalize(d Description) (Description, error) {
	for _, f := range []*string{&d.Model, &d.Summary, &d.SourceURL, &d.License, &d.Parameters, &d.Quantisation} {
		*f = strings.TrimSpace(*f)
	}
	d.Description = strings.TrimSpace(d.Description)
	switch {
	case d.Model == "":
		return d, &Invalid{msg.T("Der Modellname fehlt.", "The model name is missing.")}
	case utf8.RuneCountInString(d.Model) > MaxModelLen || strings.ContainsAny(d.Model, " \t\r\n"):
		return d, &Invalid{msg.T("Der Modellname ist ungültig.", "The model name is invalid.")}
	case utf8.RuneCountInString(d.Summary) > MaxSummaryLen:
		return d, &Invalid{msg.F("Die Kurzbeschreibung hat mehr als %d Zeichen.", "The summary has more than %d characters.", MaxSummaryLen)}
	case utf8.RuneCountInString(d.Description) > MaxDescriptionLen:
		return d, &Invalid{msg.F("Die Beschreibung hat mehr als %d Zeichen.", "The description has more than %d characters.", MaxDescriptionLen)}
	case utf8.RuneCountInString(d.License) > MaxShortLen || utf8.RuneCountInString(d.Parameters) > MaxShortLen || utf8.RuneCountInString(d.Quantisation) > MaxShortLen:
		return d, &Invalid{msg.F("Lizenz, Parameter und Quantisierung haben höchstens %d Zeichen.", "Licence, parameters and quantisation have at most %d characters.", MaxShortLen)}
	}
	if d.SourceURL != "" {
		u, err := url.Parse(d.SourceURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(d.SourceURL) > MaxURLLen {
			return d, &Invalid{msg.T("Die Herkunft muss eine http- oder https-Adresse sein.", "The source must be an http or https URL.")}
		}
	}
	return d, nil
}

// Store keeps the descriptions.
type Store interface {
	List(ctx context.Context) ([]Description, error)
	Save(ctx context.Context, d Description) error
	// Delete removes a description; one that does not exist is no error.
	Delete(ctx context.Context, model string) error
}

// MemoryStore is for development and tests.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]Description
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{rows: map[string]Description{}} }

func (s *MemoryStore) List(context.Context) ([]Description, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Description, 0, len(s.rows))
	for _, d := range s.rows {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

func (s *MemoryStore) Save(_ context.Context, d Description) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[d.Model] = d
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, model)
	return nil
}

// GormStore keeps them in the table model_descriptions.
type GormStore struct{ db *gorm.DB }

// NewGormStore creates or migrates the table.
func NewGormStore(db *gorm.DB) (*GormStore, error) {
	if err := db.AutoMigrate(&Description{}); err != nil {
		return nil, err
	}
	return &GormStore{db: db}, nil
}

func (s *GormStore) List(ctx context.Context) ([]Description, error) {
	out := []Description{}
	return out, s.db.WithContext(ctx).Order("model").Find(&out).Error
}

func (s *GormStore) Save(ctx context.Context, d Description) error {
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&d).Error
}

func (s *GormStore) Delete(ctx context.Context, model string) error {
	return s.db.WithContext(ctx).Delete(&Description{}, "model = ?", model).Error
}

// ByModel indexes descriptions by their model name.
func ByModel(list []Description) map[string]Description {
	out := make(map[string]Description, len(list))
	for _, d := range list {
		out[d.Model] = d
	}
	return out
}
