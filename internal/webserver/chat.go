package webserver

import (
	"bytes"
	"context"
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

	"github.com/pfisterer/llm-management-api/internal/access"
	"github.com/pfisterer/llm-management-api/internal/keys"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// ChatOptions configures the chat exchange.
type ChatOptions struct {
	Access    *access.Service
	Keys      *keys.Service
	Tiers     map[string]keys.Tier
	Gateway   string // LiteLLM inference gateway, e.g. http://litellm-gateway:4000
	MasterKey string // derives the chat keys (see keys.ChatKey)
	Log       *zap.SugaredLogger
}

// chatKeyTTL is how long a person's access and key count as checked. Every
// expiry asks the access rules again — not only when the key is created, or
// whoever lost their rule would keep chatting as long as the key exists.
const chatKeyTTL = 10 * time.Minute

// Up to 64 MiB: a document travels base64-encoded, a third larger than the file.
const chatBodyLimit = 64 << 20

// ChatRouter serves inference for the chat UI (LibreChat) under the LiteLLM key
// of the signed-in person. LibreChat calls LiteLLM server-side, so it can only
// use a personal key if something on this path swaps it in: LibreChat sends the
// identity as headers ({{LIBRECHAT_USER_OPENIDID}} = Keycloak sub,
// {{LIBRECHAT_USER_EMAIL}}), and this exchange answers with the person's key.
// Tier, budget and limits therefore apply exactly as for their own API keys, and
// spend lands with the right person.
//
// Trust in those headers rests on the NetworkPolicy alone: only the LibreChat
// pod reaches this listener. That is why it is a listener of its own.
func ChatRouter(o ChatOptions) http.Handler {
	target, err := url.Parse(strings.TrimRight(o.Gateway, "/"))
	if err != nil || target.Host == "" {
		panic("chat exchange: invalid gateway URL " + o.Gateway)
	}
	ex := &chatExchange{o: o, ok: map[string]time.Time{}}
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

type chatExchange struct {
	o     ChatOptions
	proxy *httputil.ReverseProxy
	mu    sync.Mutex
	ok    map[string]time.Time // user id -> checked until
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
	until := ex.ok[uid]
	ex.mu.Unlock()
	if time.Now().Before(until) {
		return keys.ChatKey(ex.o.MasterKey, uid), 0, ""
	}

	type answer struct {
		key, msg string
		status   int
	}
	// Detached from the request: the shared call must not die with whichever
	// request happened to start it.
	v, _, _ := ex.group.Do(uid, func() (any, error) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		d, _, err := ex.o.Access.Decide(cctx, p.Email)
		if err != nil {
			ex.o.Log.Errorw("chat: access decision failed", "email", p.Email, "error", err)
			return answer{status: http.StatusServiceUnavailable, msg: "Zugriff konnte gerade nicht geprüft werden — bitte gleich noch einmal versuchen"}, nil
		}
		if d.Role == access.RoleNone {
			return answer{status: http.StatusForbidden, msg: "Kein Zugang zum LLM-Dienst. Den Zugang vergibt die Administration des Dienstes."}, nil
		}
		tier, ok := ex.o.Tiers[d.Tier]
		if !ok {
			ex.o.Log.Errorw("chat: tier not configured", "tier", d.Tier, "email", p.Email)
			return answer{status: http.StatusInternalServerError, msg: "Kontingentklasse nicht konfiguriert"}, nil
		}
		key, err := ex.o.Keys.EnsureChatKey(cctx, p, tier, ex.o.MasterKey)
		if err != nil {
			ex.o.Log.Errorw("chat: key not available", "user", uid, "error", err)
			return answer{status: http.StatusBadGateway, msg: "Key konnte nicht angelegt werden"}, nil
		}
		ex.mu.Lock()
		ex.ok[uid] = time.Now().Add(chatKeyTTL)
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
