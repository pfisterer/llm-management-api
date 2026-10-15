package webserver

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// ChatOptions configures the chat exchange.
type ChatOptions struct {
	Access *access.Service
	Keys   *keys.Service
	// Quota decides the pools (from the projects).
	Quota     LLMQuota
	Gateway   string // LiteLLM inference gateway, e.g. http://litellm-gateway:4000
	MasterKey string // derives the chat keys (see keys.ChatKey)
	// Token is the shared secret LibreChat sends as its API key (Authorization: Bearer <token>, from its Secret); empty = every request is refused.
	Token string
	Log   *zap.SugaredLogger
}

// chatKeyTTL is how long a person's access and key count as checked. Every expiry asks the projects again — not only when the key is created, or whoever left their project would keep chatting as long as the key exists.
const chatKeyTTL = 10 * time.Minute

// Up to 64 MiB: a document travels base64-encoded, a third larger than the file.
const chatBodyLimit = 64 << 20

// ChatRouter serves inference for the chat UI (LibreChat) under the LiteLLM key
// of the signed-in person. LibreChat calls LiteLLM server-side, so it can only
// use a personal key if something on this path swaps it in: LibreChat sends the
// identity as headers ({{LIBRECHAT_USER_OPENIDID}} = Keycloak sub,
// {{LIBRECHAT_USER_EMAIL}}), and this exchange answers with the person's key.
// The project's limits therefore apply exactly as for the person's own API keys in it, and spend lands in the project's pool.
//
// Trust in those headers rests on two things: the shared token LibreChat sends as its API key (Token, compared in constant time), and the NetworkPolicy, by which only the LibreChat pod reaches this listener (a listener of its own for that). The token keeps out what else runs in that pod on behalf of people, e.g. HTTP calls of agents. On top, sub and e-mail must belong together (keys.CheckIdentity).
func ChatRouter(o ChatOptions) http.Handler {
	target, err := url.Parse(strings.TrimRight(o.Gateway, "/"))
	if err != nil || target.Host == "" {
		panic("chat exchange: invalid gateway URL " + o.Gateway)
	}
	ex := &chatExchange{o: o, ok: map[string]chatChecked{}}
	ex.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
		},
		// Pass server-sent events through as they come: a buffered stream would
		// be minutes of nothing for the person waiting.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			// Key deleted in LiteLLM meanwhile? Check again next time instead of
			// answering 401 until the cache runs out.
			if resp.StatusCode == http.StatusUnauthorized {
				if uid, _ := resp.Request.Context().Value(chatUIDKey{}).(string); uid != "" {
					ex.forget(uid)
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // the browser went away; nothing to answer
			}
			o.Log.Errorw("chat: gateway unreachable", "error", err)
			chatError(w, http.StatusBadGateway, "Gateway nicht erreichbar")
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("/v1/", ex.serve)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { chatError(w, http.StatusNotFound, "not found") })
	return mux
}

type chatUIDKey struct{}

// chatChecked is a person whose access and key were checked, with the e-mail that came with the sub: another e-mail is checked anew.
type chatChecked struct {
	until time.Time
	email string
}

type chatExchange struct {
	o     ChatOptions
	proxy *httputil.ReverseProxy
	mu    sync.Mutex
	ok    map[string]chatChecked // user id -> checked until, for which e-mail
	// Two requests of one person at once: only one creates the key, or the
	// second fails on the key the first has just taken.
	group singleflight.Group
}

func (ex *chatExchange) forget(uid string) {
	ex.mu.Lock()
	delete(ex.ok, uid)
	ex.mu.Unlock()
}

func (ex *chatExchange) serve(w http.ResponseWriter, r *http.Request) {
	raw, ok := authn.CutBearerPrefix(r.Header.Get("Authorization"))
	if !ok || ex.o.Token == "" || subtle.ConstantTimeCompare([]byte(raw), []byte(ex.o.Token)) != 1 {
		chatError(w, http.StatusUnauthorized, "Token fehlt oder ist falsch.")
		return
	}
	sub := strings.TrimSpace(r.Header.Get("X-Librechat-User-Openid"))
	email := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Librechat-User-Email")))
	// An unreplaced placeholder means LibreChat did not know the person (or its
	// configuration is wrong). Never let that pass as an identity.
	if sub == "" || strings.Contains(sub, "{{") || strings.Contains(email, "{{") {
		chatError(w, http.StatusUnauthorized, "keine Identität von LibreChat erhalten")
		return
	}
	if email == "" {
		chatError(w, http.StatusUnauthorized, "keine E-Mail-Adresse von LibreChat erhalten")
		return
	}

	if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
		raw, err := io.ReadAll(io.LimitReader(r.Body, chatBodyLimit+1))
		if err != nil || len(raw) > chatBodyLimit {
			chatError(w, http.StatusRequestEntityTooLarge, "Die Anfrage ist zu groß.")
			return
		}
		body, rejected := checkAttachments(raw)
		if rejected != "" {
			chatError(w, http.StatusBadRequest, "„"+rejected+"“ wurde direkt an das Modell geschickt — das können die "+
				"Modelle hier nicht lesen. Bitte die Datei über die Büroklammer mit „Hochladen als Text“ "+
				"oder „Hochladen für Dateisuche“ anhängen.")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
		r.Header.Del("Transfer-Encoding")
	}

	p := keys.Person{Subject: sub, Email: email}
	key, status, msg := ex.key(r.Context(), p)
	if key == "" {
		chatError(w, status, msg)
		return
	}
	for _, h := range []string{"Authorization", "X-Librechat-User-Openid", "X-Librechat-User-Email"} {
		r.Header.Del(h)
	}
	r.Header.Set("Authorization", "Bearer "+key)
	ex.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), chatUIDKey{}, p.UserID())))
}

// key returns the person's chat key, or a status and message for the chat.
func (ex *chatExchange) key(ctx context.Context, p keys.Person) (string, int, string) {
	uid := p.UserID()
	ex.mu.Lock()
	seen := ex.ok[uid]
	ex.mu.Unlock()
	if seen.email == p.Email && time.Now().Before(seen.until) {
		return keys.ChatKey(ex.o.MasterKey, uid), 0, ""
	}

	type answer struct {
		key, msg string
		status   int
	}
	// Detached from the request: the shared call must not die with whichever
	// request happened to start it.
	v, _, _ := ex.group.Do(uid+"\n"+p.Email, func() (any, error) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		// Sub and e-mail come as claims from LibreChat; they must be one person's, or one could spend another's pool under their own e-mail's projects, or the other way round.
		if err := ex.o.Keys.CheckIdentity(cctx, p); errors.Is(err, keys.ErrIdentityMismatch) {
			ex.o.Log.Warnw("chat: sub and e-mail do not belong together", "user", uid, "email", p.Email)
			return answer{status: http.StatusForbidden, msg: "Konto und E-Mail-Adresse passen nicht zusammen."}, nil
		} else if err != nil {
			ex.o.Log.Errorw("chat: identity not checked", "user", uid, "error", err)
			return answer{status: http.StatusBadGateway, msg: "LiteLLM nicht erreichbar"}, nil
		}
		g, status, msg := ex.o.Quota.GrantFor(cctx, ex.o.Access.Who(cctx, p.Email).Tokens)
		if status != 0 {
			return answer{status: status, msg: msg}, nil
		}
		key, err := ex.o.Keys.EnsureChatKey(cctx, p, g, ex.o.MasterKey)
		if err != nil {
			ex.o.Log.Errorw("chat: key not available", "user", uid, "error", err)
			return answer{status: http.StatusBadGateway, msg: "Key konnte nicht angelegt werden"}, nil
		}
		ex.mu.Lock()
		ex.ok[uid] = chatChecked{until: time.Now().Add(chatKeyTTL), email: p.Email}
		ex.mu.Unlock()
		return answer{key: key}, nil
	})
	a := v.(answer)
	return a.key, a.status, a.msg
}

// chatError answers in OpenAI's error format — that is how LibreChat shows the
// text in the conversation.
func chatError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": "invalid_request_error", "code": status},
	})
}

var docParts = map[string]bool{"file": true, "input_file": true, "document": true}

// checkAttachments looks for documents attached as a whole ("upload to
// provider"). LibreChat always offers that path for custom endpoints and sends
// the file as a content part of type "file". OpenAI and Anthropic understand
// it, Ollama's OpenAI API does not and answers a meaningless "invalid message
// format" (06.10.2026). It cannot be switched off in LibreChat v0.8.7, so it is
// caught here.
//
// Only the NEWEST user message is rejected (rejected = file name). LibreChat
// resends the whole history with every answer, so rejecting any earlier one
// would block the conversation for good; earlier attachments are replaced by a
// note instead, and the body comes back rewritten.
func checkAttachments(raw []byte) (body []byte, rejected string) {
	var req map[string]any
	if json.Unmarshal(raw, &req) != nil {
		return raw, ""
	}
	messages, _ := req["messages"].([]any)
	lastUser := -1
	for i, m := range messages {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "user" {
			lastUser = i
		}
	}
	changed := false
	for i, m := range messages {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := mm["content"].([]any)
		if !ok {
			continue
		}
		for j, part := range parts {
			pm, ok := part.(map[string]any)
			if !ok || !docParts[str(pm["type"])] {
				continue
			}
			if i == lastUser {
				return raw, docName(pm)
			}
			parts[j] = map[string]any{"type": "text", "text": "[Anhang „" + docName(pm) + "“ wurde nicht an das Modell übergeben.]"}
			changed = true
		}
	}
	if !changed {
		return raw, ""
	}
	out, err := json.Marshal(req)
	if err != nil {
		return raw, ""
	}
	return out, ""
}

func docName(part map[string]any) string {
	if f, ok := part["file"].(map[string]any); ok {
		if n := str(f["filename"]); n != "" {
			return n
		}
	}
	if n := str(part["filename"]); n != "" {
		return n
	}
	return "Datei"
}

func str(v any) string { s, _ := v.(string); return s }
