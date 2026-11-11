package webserver

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/page"
)

// jsonNames are the JSON names of t's fields.
func jsonNames(t reflect.Type) []string {
	var out []string
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// The API description shows a paged answer as Page{items=[]T}; the handlers answer with golib's page.Page. Both have to be the same JSON.
func TestPageDocMatchesGolib(t *testing.T) {
	doc, golib := jsonNames(reflect.TypeFor[Page]()), jsonNames(reflect.TypeFor[page.Page[int]]())
	if !slices.Equal(doc, golib) {
		t.Fatalf("webserver.Page has %v, page.Page %v", doc, golib)
	}
}

// A list pages by limit and offset and says how many there are; bounds that do not parse are a 400.
func TestListsArePaged(t *testing.T) {
	h, _, _ := tokenServer(t, 10)
	for range 3 {
		issue(t, h, "student@dhbw.de", CreateAPITokenRequest{Description: "x"})
	}
	code, body := call(t, h, "GET", "/v1/tokens?limit=2&offset=1", "oidc:student@dhbw.de", "", nil)
	var p page.Page[APIToken]
	if _ = json.Unmarshal(body, &p); code != http.StatusOK || len(p.Items) != 2 || p.Total != 3 || p.Limit != 2 || p.Offset != 1 {
		t.Fatalf("page 2 of 3 tokens: %d %s", code, body)
	}
	code, body = call(t, h, "GET", "/v1/tokens?offset=-1", "oidc:student@dhbw.de", "", nil)
	if code != http.StatusBadRequest || !json.Valid(body) {
		t.Fatalf("a negative offset: %d %s", code, body)
	}
}
