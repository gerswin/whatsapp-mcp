package mcpserver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	redirectURI = "https://claude.ai/api/mcp/auth_callback"
	verifier    = "a-sufficiently-long-pkce-code-verifier-0123456789"
)

// noRedirect lets tests inspect the 302 the authorization endpoint issues.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func challenge() string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func decode(t *testing.T, resp *http.Response, wantStatus int) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: status %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, wantStatus, body)
	}
	out := map[string]any{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func getJSON(t *testing.T, target string) map[string]any {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, resp, http.StatusOK)
}

func postForm(t *testing.T, target string, form url.Values) *http.Response {
	t.Helper()
	resp, err := noRedirect.PostForm(target, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func registerClient(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := http.Post(srv.URL+pathRegister, "application/json",
		strings.NewReader(`{"client_name":"Claude","redirect_uris":["`+redirectURI+`"],"token_endpoint_auth_method":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
	client := decode(t, resp, http.StatusCreated)
	id, _ := client["client_id"].(string)
	if id == "" {
		t.Fatalf("no client_id in %v", client)
	}
	if _, hasSecret := client["client_secret"]; hasSecret {
		t.Fatalf("public client must not get a secret: %v", client)
	}
	return id
}

func authForm(clientID string) url.Values {
	return url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {"st4te"},
		"code_challenge":        {challenge()},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
	}
}

// authorize submits the consent form with an account token and returns the
// authorization code delivered to the redirect URI.
func authorize(t *testing.T, srv *httptest.Server, clientID, accountToken string) string {
	t.Helper()
	form := authForm(clientID)
	form.Set("account_token", accountToken)
	form.Set("action", "allow")

	resp := postForm(t, srv.URL+pathAuthorize, form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: status %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), redirectURI) {
		t.Fatalf("authorize redirected to %q", resp.Header.Get("Location"))
	}
	if loc.Query().Get("state") != "st4te" {
		t.Fatalf("state not echoed: %s", loc)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in %s", loc)
	}
	return code
}

func exchange(t *testing.T, srv *httptest.Server, clientID, code, codeVerifier string) *http.Response {
	t.Helper()
	return postForm(t, srv.URL+pathToken, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {codeVerifier},
	})
}

func TestOAuthDiscovery(t *testing.T) {
	srv, _ := startServer(t, nil)

	// The 401 must point at the protected-resource metadata.
	resp, err := http.Post(srv.URL+Path, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := `resource_metadata="` + srv.URL + pathResourceMetadata + `"`
	if got := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(got, want) {
		t.Fatalf("status %d, WWW-Authenticate %q; want 401 containing %s", resp.StatusCode, got, want)
	}

	for _, path := range []string{pathResourceMetadata, pathResourceMetadata + Path} {
		meta := getJSON(t, srv.URL+path)
		servers, _ := meta["authorization_servers"].([]any)
		if meta["resource"] != srv.URL+Path || len(servers) != 1 || servers[0] != srv.URL {
			t.Fatalf("%s: unexpected metadata %v", path, meta)
		}
	}

	meta := getJSON(t, srv.URL+pathServerMetadata)
	for field, want := range map[string]string{
		"issuer":                 srv.URL,
		"authorization_endpoint": srv.URL + pathAuthorize,
		"token_endpoint":         srv.URL + pathToken,
		"registration_endpoint":  srv.URL + pathRegister,
	} {
		if meta[field] != want {
			t.Errorf("%s = %v, want %s", field, meta[field], want)
		}
	}
}

func TestOAuthPublicURLBehindProxy(t *testing.T) {
	srv, _ := startServer(t, nil)

	// A tunnel such as cloudflared terminates TLS and forwards these headers.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+pathServerMetadata, nil)
	req.Host = "wa.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if meta := decode(t, resp, http.StatusOK); meta["issuer"] != "https://wa.example.com" || meta["token_endpoint"] != "https://wa.example.com/oauth/token" {
		t.Fatalf("forwarded headers ignored: %v", meta)
	}
}

func TestOAuthFullFlow(t *testing.T) {
	srv, _ := startServer(t, seededStore(t))
	clientID := registerClient(t, srv)

	// The consent page renders for a valid request and names the client.
	resp, err := http.Get(srv.URL + pathAuthorize + "?" + authForm(clientID).Encode())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), "Claude") || !strings.Contains(string(page), "claude.ai") {
		t.Fatalf("consent page: status %d, body %.200s", resp.StatusCode, page)
	}

	// A wrong account token re-renders the form instead of issuing a code.
	bad := authForm(clientID)
	bad.Set("account_token", "nope")
	resp = postForm(t, srv.URL+pathAuthorize, bad)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: status %d, want 401", resp.StatusCode)
	}

	code := authorize(t, srv, clientID, tokenA)
	tokens := decode(t, exchange(t, srv, clientID, code, verifier), http.StatusOK)
	access, _ := tokens["access_token"].(string)
	refresh, _ := tokens["refresh_token"].(string)
	if !strings.HasPrefix(access, accessTokenPrefix) || refresh == "" || tokens["token_type"] != "Bearer" {
		t.Fatalf("unexpected token response: %v", tokens)
	}

	// The access token opens an MCP session bound to the authorized account.
	session := connect(t, srv, "Authorization", "Bearer "+access)
	var status statusOutput
	call(t, session, "get_status", map[string]any{}, &status)
	if status.Account != "inst-a" {
		t.Fatalf("OAuth session bound to %q, want inst-a", status.Account)
	}
	var found searchMessagesOutput
	call(t, session, "search_messages", map[string]any{"query": "stock"}, &found)
	if len(found.Messages) != 1 || found.Messages[0].ID != "m1" {
		t.Fatalf("OAuth session saw another account's data: %+v", found.Messages)
	}

	// Codes are single use.
	decode(t, exchange(t, srv, clientID, code, verifier), http.StatusBadRequest)

	// Refresh rotates: the new pair works, the old refresh token is dead.
	refreshForm := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refresh}}
	rotated := decode(t, postForm(t, srv.URL+pathToken, refreshForm), http.StatusOK)
	if rotated["access_token"] == access || rotated["refresh_token"] == refresh || rotated["access_token"] == "" {
		t.Fatalf("refresh did not rotate tokens: %v", rotated)
	}
	decode(t, postForm(t, srv.URL+pathToken, refreshForm), http.StatusBadRequest)

	// A refresh token is not an access token.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+Path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+refresh)
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh token accepted as access token (status %d)", resp.StatusCode)
	}

	// Removing the account revokes its grants: neither token works any more.
	if err := revokeAccount("inst-a"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	req, _ = http.NewRequest(http.MethodPost, srv.URL+Path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+rotated["access_token"].(string))
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("access token survived revocation (status %d)", resp.StatusCode)
	}
	revokedRefresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {rotated["refresh_token"].(string)}}
	decode(t, postForm(t, srv.URL+pathToken, revokedRefresh), http.StatusBadRequest)
}

func TestOAuthRejectsBadRequests(t *testing.T) {
	srv, _ := startServer(t, nil)
	clientID := registerClient(t, srv)

	// Wrong PKCE verifier: no tokens, and the code is burned.
	code := authorize(t, srv, clientID, tokenA)
	if got := decode(t, exchange(t, srv, clientID, code, "wrong-verifier"), http.StatusBadRequest); got["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %v", got)
	}
	decode(t, exchange(t, srv, clientID, code, verifier), http.StatusBadRequest)

	// A code issued to one client cannot be redeemed by another.
	other := registerClient(t, srv)
	code = authorize(t, srv, clientID, tokenA)
	decode(t, exchange(t, srv, other, code, verifier), http.StatusBadRequest)

	// Unregistered redirect URIs are never redirected to.
	evil := authForm(clientID)
	evil.Set("redirect_uri", "https://evil.example/cb")
	evil.Set("account_token", tokenA)
	resp := postForm(t, srv.URL+pathAuthorize, evil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Fatalf("unregistered redirect_uri: status %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// PKCE is mandatory; the error goes back to the registered redirect URI.
	noPKCE := authForm(clientID)
	noPKCE.Del("code_challenge")
	resp, err := noRedirect.Get(srv.URL + pathAuthorize + "?" + noPKCE.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || !strings.Contains(loc, "error=invalid_request") {
		t.Fatalf("missing PKCE: status %d, Location %q", resp.StatusCode, loc)
	}

	// Denying consent reports access_denied without needing a token.
	deny := authForm(clientID)
	deny.Set("action", "deny")
	resp = postForm(t, srv.URL+pathAuthorize, deny)
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "error=access_denied") {
		t.Fatalf("deny: Location %q", loc)
	}

	// Registration refuses non-https, non-loopback redirect URIs.
	for uri, want := range map[string]int{
		"http://evil.example/cb":     http.StatusBadRequest,
		"javascript:alert(1)":        http.StatusBadRequest,
		"http://127.0.0.1:8765/cb":   http.StatusCreated,
		"https://chatgpt.com/any/cb": http.StatusCreated,
	} {
		resp, err := http.Post(srv.URL+pathRegister, "application/json", strings.NewReader(`{"redirect_uris":["`+uri+`"]}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("register %s: status %d, want %d", uri, resp.StatusCode, want)
		}
	}
}

func TestOAuthConfidentialClient(t *testing.T) {
	srv, _ := startServer(t, nil)

	resp, err := http.Post(srv.URL+pathRegister, "application/json",
		strings.NewReader(`{"redirect_uris":["`+redirectURI+`"],"token_endpoint_auth_method":"client_secret_post"}`))
	if err != nil {
		t.Fatal(err)
	}
	client := decode(t, resp, http.StatusCreated)
	clientID, _ := client["client_id"].(string)
	secret, _ := client["client_secret"].(string)
	if secret == "" {
		t.Fatalf("confidential client got no secret: %v", client)
	}

	form := func(code, clientSecret string) url.Values {
		return url.Values{
			"grant_type": {"authorization_code"}, "client_id": {clientID}, "client_secret": {clientSecret},
			"code": {code}, "code_verifier": {verifier},
		}
	}

	code := authorize(t, srv, clientID, tokenA)
	decode(t, postForm(t, srv.URL+pathToken, form(code, "wrong")), http.StatusUnauthorized)

	// A failed client authentication must not burn the code.
	decode(t, postForm(t, srv.URL+pathToken, form(code, secret)), http.StatusOK)
}
