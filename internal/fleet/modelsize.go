package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ModelSize is the download size of an Ollama model in bytes (weights, projector and the rest), -1 when it cannot be told (a model from another registry than Ollama's, e.g. hf.co/…). ErrUnknownModel when the registry has no such model.
type ModelSize func(ctx context.Context, model string) (int64, error)

var (
	ErrUnknownModel = errors.New("model not in the Ollama registry")
	// ErrRegistry: the registry did not answer, the size is unknown.
	ErrRegistry = errors.New("ollama registry not reachable")
)

// kvReserveMb is what a machine needs on top of the weights (KV cache, compute buffers) for the rough check whether a model fits its memory limit. The machine's own attempt to load it reports what the check misses (Peer.ModelError).
const kvReserveMb = 1024

// OllamaRegistry asks registry.ollama.ai for a model's manifest and adds up its layers.
func OllamaRegistry(client *http.Client) ModelSize {
	return func(ctx context.Context, model string) (int64, error) {
		name, tag, _ := strings.Cut(model, ":")
		if tag == "" {
			tag = "latest"
		}
		first, _, _ := strings.Cut(name, "/")
		if strings.Contains(first, ".") {
			return -1, nil // another registry
		}
		if !strings.Contains(name, "/") {
			name = "library/" + name
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://registry.ollama.ai/v2/"+name+"/manifests/"+tag, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
		res, err := client.Do(req)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrRegistry, err)
		}
		defer res.Body.Close()
		if res.StatusCode == http.StatusNotFound {
			return 0, ErrUnknownModel
		}
		if res.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("%w: HTTP %d", ErrRegistry, res.StatusCode)
		}
		var m struct {
			Layers []struct {
				Size int64 `json:"size"`
			} `json:"layers"`
		}
		if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
			return 0, fmt.Errorf("%w: %v", ErrRegistry, err)
		}
		var n int64
		for _, l := range m.Layers {
			n += l.Size
		}
		return n, nil
	}
}

// MisfitError names the machines whose memory limit a model exceeds.
type MisfitError struct {
	Model    string
	SizeGB   float64
	Machines []string // "wimac05 (14 GB)"
}

func (e *MisfitError) Error() string {
	return fmt.Sprintf("%s (%.1f GB) passt nicht auf %s", e.Model, e.SizeGB, strings.Join(e.Machines, ", "))
}
