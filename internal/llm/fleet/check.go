package fleet

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"slices"
)

// CheckResponse is the answer of the light check: only what decides whether the machine should run its full enrolment now. Field name as in EnrollResponse, so the script reads both the same way with plutil.
type CheckResponse struct {
	ScriptManifest string `json:"scriptManifest"`
}

// Check is the light check a Mac runs every few minutes between its hourly enrolments (dhbw-llm-enroll.sh --check): the script manifest this machine should run, computed exactly as in its enrolment answer (fleet.selfUpdate and fleet.selfUpdateCanary), so the machine can start a full enrolment as soon as a new script is delivered instead of at its next hourly run.
//
// Same authentication as enrolment (the shared enrolment token at the route, the machine secret here), but stricter: only an approved, unblocked machine that already holds a secret is answered. A check never registers a machine and never takes the place of an enrolment, so there is no migration case to serve, and unknown, pending, blocked and unauthenticated callers all get the same 403 (the check reveals nothing about which serials exist).
//
// Read-only on purpose: it does not touch the peer record, not even last_seen. last_seen means "a full enrolment got through" (tunnel material, model assignment, reports); a check succeeding while the full run fails would hide exactly that in the portal, and a write every few minutes per machine would buy nothing.
func (s *Service) Check(ctx context.Context, req EnrollRequest) (CheckResponse, error) {
	if !s.cfg.Enabled {
		return CheckResponse{}, fail(503, "fleet enrollment disabled")
	}
	serial := req.str("serial")
	if !serialRE.MatchString(serial) {
		return CheckResponse{}, fail(400, "invalid serial")
	}
	denied := fail(403, "unknown, blocked or unauthenticated machine")
	if len(s.cfg.AllowedSerials) > 0 && !slices.Contains(s.cfg.AllowedSerials, serial) {
		return CheckResponse{}, denied
	}
	peers, err := s.store.List(ctx)
	if err != nil {
		return CheckResponse{}, err
	}
	i := slices.IndexFunc(peers, func(p Peer) bool { return p.Serial == serial })
	if i < 0 {
		return CheckResponse{}, denied
	}
	p := peers[i]
	if p.Blocked || p.Pending || !secretMatches(p.SecretHash, req.str("machineSecret")) {
		return CheckResponse{}, denied
	}
	return CheckResponse{ScriptManifest: s.scripts.Manifest(s.cfg, serial)}, nil
}

// secretMatches compares a presented machine secret with the stored hash in constant time; no stored hash never matches.
func secretMatches(storedHash, secret string) bool {
	want, err := hex.DecodeString(storedHash)
	if err != nil || len(want) != sha256.Size || secret == "" {
		return false
	}
	got := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(want, got[:]) == 1
}
