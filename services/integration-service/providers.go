package integrations

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/bh-premnath-git/LawPilot/packages/workload"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Keyring struct {
	Active string            `json:"active"`
	Keys   map[string]string `json:"keys"`
}

func LoadKeys() (*Keyring, error) {
	raw := workload.Secret("INTEGRATION_TOKEN_KEYS")
	var k Keyring
	if json.Unmarshal([]byte(raw), &k) != nil || k.Active == "" {
		return nil, errors.New("token keyring required")
	}
	for _, key := range k.Keys {
		b, err := base64.StdEncoding.DecodeString(key)
		if err != nil || len(b) != 32 {
			return nil, errors.New("AES256 key required")
		}
	}
	if k.Keys[k.Active] == "" {
		return nil, errors.New("active key missing")
	}
	return &k, nil
}
func (k *Keyring) Seal(plain []byte, aad string) ([]byte, error) {
	key, _ := base64.StdEncoding.DecodeString(k.Keys[k.Active])
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := aead.Seal(nonce, nonce, plain, []byte(aad))
	return json.Marshal(struct {
		Key  string `json:"key"`
		Data []byte `json:"data"`
	}{k.Active, sealed})
}
func (k *Keyring) Open(encrypted []byte, aad string) ([]byte, error) {
	var envelope struct {
		Key  string `json:"key"`
		Data []byte `json:"data"`
	}
	if json.Unmarshal(encrypted, &envelope) != nil {
		return nil, errors.New("invalid credential envelope")
	}
	key, err := base64.StdEncoding.DecodeString(k.Keys[envelope.Key])
	if err != nil || len(key) != 32 {
		return nil, errors.New("unknown key")
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	if len(envelope.Data) < aead.NonceSize() {
		return nil, errors.New("invalid credential envelope")
	}
	plain, err := aead.Open(nil, envelope.Data[:aead.NonceSize()], envelope.Data[aead.NonceSize():], []byte(aad))
	if err != nil {
		return nil, errors.New("credential integrity failure")
	}
	return plain, nil
}

type Tokens struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	ExpiresIn int64  `json:"expires_in"`
	Type      string `json:"token_type"`
}
type Provider struct {
	ID, Base, AuthorizeURL, TokenURL, ClientID, ClientSecret, Redirect, Version, TimeZone string
	HTTP                                                                                  *http.Client
}

func NewProviders() (map[string]*Provider, *Keyring, error) {
	out := map[string]*Provider{}
	for _, name := range []string{"CLIO", "GENERIC_PROVIDER"} {
		enabled, err := requiredFeature(name + "_ENABLED")
		if err != nil {
			return nil, nil, err
		}
		if !enabled {
			continue
		}
		p := &Provider{ClientID: os.Getenv(name + "_CLIENT_ID"), ClientSecret: workload.Secret(name + "_CLIENT_SECRET"), Redirect: os.Getenv(name + "_REDIRECT_URI"), Version: os.Getenv(name + "_API_VERSION"), TimeZone: os.Getenv(name + "_TIME_ZONE"), HTTP: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("provider redirect forbidden") }}}
		if name == "CLIO" {
			region := os.Getenv("CLIO_REGION")
			if region == "" {
				region = "us"
			}
			host := map[string]string{"us": "https://app.clio.com", "eu": "https://eu.app.clio.com", "ca": "https://ca.app.clio.com", "au": "https://au.app.clio.com"}[region]
			if host == "" {
				return nil, nil, errors.New("invalid Clio region")
			}
			p.ID = "clio-manage"
			p.Base = host + "/api/v4"
			p.AuthorizeURL = host + "/oauth/authorize"
			p.TokenURL = host + "/oauth/token"
		} else {
			p.ID = "generic-rest"
			p.Base = strings.TrimRight(os.Getenv(name+"_BASE_URL"), "/")
			p.AuthorizeURL = os.Getenv(name + "_AUTHORIZE_URL")
			p.TokenURL = os.Getenv(name + "_TOKEN_URL")
		}
		for _, value := range []string{p.Base, p.AuthorizeURL, p.TokenURL, p.Redirect} {
			if err = endpoint(value); err != nil {
				return nil, nil, err
			}
		}
		if p.TimeZone == "" {
			return nil, nil, errors.New("provider account IANA time zone required")
		}
		if _, err = time.LoadLocation(p.TimeZone); err != nil {
			return nil, nil, errors.New("invalid provider account zone")
		}
		if p.ClientID == "" || p.ClientSecret == "" {
			return nil, nil, errors.New("OAuth client configuration required")
		}
		out[p.ID] = p
	}
	if len(out) == 0 {
		return out, nil, nil
	}
	keys, err := LoadKeys()
	return out, keys, err
}
func (p *Provider) Exchange(ctx context.Context, code, refresh string) (Tokens, error) {
	form := url.Values{"client_id": {p.ClientID}, "client_secret": {p.ClientSecret}}
	if refresh != "" {
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", refresh)
	} else {
		form.Set("grant_type", "authorization_code")
		form.Set("code", code)
		form.Set("redirect_uri", p.Redirect)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, errors.New("token request invalid")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return Tokens{}, errors.New("token request unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Tokens{}, errors.New("token request rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return Tokens{}, errors.New("token response invalid")
	}
	var tokens Tokens
	if json.Unmarshal(raw, &tokens) != nil || tokens.Access == "" || tokens.ExpiresIn < 60 || tokens.ExpiresIn > 86400*30 || !strings.EqualFold(tokens.Type, "bearer") {
		return Tokens{}, errors.New("token response invalid")
	}
	if tokens.Refresh == "" {
		tokens.Refresh = refresh
	}
	return tokens, nil
}

type RemoteMatter struct {
	ID                 string
	ETag, Title, State string
}
type RemoteCalendar struct{ ID, ETag, Name string }
type RemoteEntry struct {
	ID, ETag, Calendar, Matter, Title, Description, Zone string
	Start, End                                           time.Time
	AllDay                                               bool
}
type RemotePage struct {
	Matters   []RemoteMatter
	Calendars []RemoteCalendar
	Entries   []RemoteEntry
	Next      string
}

func scalar(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		if _, err := strconv.ParseUint(string(n), 10, 64); err == nil {
			return string(n)
		}
	}
	return ""
}
func (p *Provider) resourcePath(kind string) string {
	if p.ID == "clio-manage" {
		return map[string]string{"matter": "/matters.json", "calendar": "/calendars.json", "calendar_entry": "/calendar_entries.json"}[kind]
	}
	return map[string]string{"matter": "/matters", "calendar": "/calendars", "calendar_entry": "/calendar-entries"}[kind]
}
func (p *Provider) safeURL(kind, cursor string) (string, error) {
	base, err := url.Parse(p.Base + p.resourcePath(kind))
	if err != nil {
		return "", err
	}
	if cursor == "" {
		q := base.Query()
		q.Set("limit", "100")
		if p.ID == "clio-manage" {
			q.Set("fields", map[string]string{"matter": "id,etag,description,status,display_number", "calendar": "id,etag,name,type", "calendar_entry": "id,etag,summary,description,start_at,end_at,all_day,calendar_owner{id,type},matter{id}"}[kind])
			q.Set("order", "id(asc)")
		}
		base.RawQuery = q.Encode()
		return base.String(), nil
	}
	next, err := url.Parse(cursor)
	if err != nil || next.Scheme != base.Scheme || next.Host != base.Host || next.Path != base.Path || next.User != nil || next.Fragment != "" || len(cursor) > 4096 {
		return "", errors.New("unsafe provider pagination")
	}
	return next.String(), nil
}
func (p *Provider) request(ctx context.Context, method, target, token, etag string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	if p.Version != "" {
		req.Header.Set("X-API-VERSION", p.Version)
	}
	return p.HTTP.Do(req)
}
func (p *Provider) Fetch(ctx context.Context, kind, cursor, token string) (RemotePage, error) {
	target, err := p.safeURL(kind, cursor)
	if err != nil {
		return RemotePage{}, err
	}
	resp, err := p.request(ctx, "GET", target, token, "", nil)
	if err != nil {
		return RemotePage{}, errors.New("provider unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return RemotePage{}, errors.New("provider retry required")
	}
	if resp.StatusCode != 200 {
		return RemotePage{}, errors.New("provider access rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 {
		return RemotePage{}, errors.New("provider page oversized")
	}
	var envelope struct {
		Data []json.RawMessage `json:"data"`
		Meta struct {
			Paging struct {
				Next string `json:"next"`
			} `json:"paging"`
		} `json:"meta"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Data) > 200 {
		return RemotePage{}, errors.New("invalid provider page")
	}
	page := RemotePage{Next: envelope.Meta.Paging.Next}
	if page.Next != "" {
		if _, err = p.safeURL(kind, page.Next); err != nil {
			return RemotePage{}, err
		}
	}
	for _, raw := range envelope.Data {
		var row struct {
			ID          json.RawMessage `json:"id"`
			ETag        string          `json:"etag"`
			Description string          `json:"description"`
			Display     string          `json:"display_number"`
			Title       string          `json:"title"`
			Name        string          `json:"name"`
			Type        string          `json:"type"`
			Status      string          `json:"status"`
			Summary     string          `json:"summary"`
			Start       string          `json:"start_at"`
			End         string          `json:"end_at"`
			AllDay      bool            `json:"all_day"`
			TimeZone    string          `json:"time_zone"`
			Recurrence  json.RawMessage `json:"recurrence_rule"`
			Deleted     bool            `json:"deleted"`
			CalendarID  json.RawMessage `json:"calendar_id"`
			MatterID    json.RawMessage `json:"matter_id"`
			Owner       struct {
				ID   json.RawMessage `json:"id"`
				Type string          `json:"type"`
			} `json:"calendar_owner"`
			Matter struct {
				ID json.RawMessage `json:"id"`
			} `json:"matter"`
		}
		if json.Unmarshal(raw, &row) != nil {
			return RemotePage{}, errors.New("invalid provider resource")
		}
		if row.Deleted || len(row.Recurrence) > 0 && string(row.Recurrence) != "null" && string(row.Recurrence) != `""` {
			return RemotePage{}, errors.New("unsupported remote deletion or recurrence")
		}
		id := scalar(row.ID)
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`).MatchString(id) || row.ETag == "" || len(row.ETag) > 200 || strings.ContainsAny(row.ETag, "\r\n") {
			return RemotePage{}, errors.New("provider identity/etag missing")
		}
		switch kind {
		case "matter":
			title := row.Title
			if p.ID == "clio-manage" {
				title = row.Description
				if title == "" {
					title = row.Display
				}
			}
			if title == "" || len(title) > 1000 {
				return RemotePage{}, errors.New("provider title invalid")
			}
			page.Matters = append(page.Matters, RemoteMatter{id, row.ETag, title, row.Status})
		case "calendar":
			if row.Name == "" || len(row.Name) > 120 {
				return RemotePage{}, errors.New("calendar name invalid")
			}
			if p.ID == "clio-manage" {
				if row.Type != "AccountCalendar" && row.Type != "UserCalendar" && row.Type != "AdhocCalendar" {
					return RemotePage{}, errors.New("unsupported Clio calendar owner")
				}
				id = row.Type + ":" + id
			}
			page.Calendars = append(page.Calendars, RemoteCalendar{id, row.ETag, row.Name})
		case "calendar_entry":
			start, e1 := time.Parse(time.RFC3339, row.Start)
			end, e2 := time.Parse(time.RFC3339, row.End)
			calendar := scalar(row.CalendarID)
			matter := scalar(row.MatterID)
			if p.ID == "clio-manage" {
				calendar = row.Owner.Type + ":" + scalar(row.Owner.ID)
				matter = scalar(row.Matter.ID)
				row.Title = row.Summary
			}
			if e1 != nil || e2 != nil || !end.After(start) || end.Sub(start) > 366*24*time.Hour || calendar == "" || row.Title == "" || len(row.Title) > 256 || len(row.Description) > 4096 {
				return RemotePage{}, errors.New("calendar interval invalid")
			}
			zone := row.TimeZone
			if zone == "" {
				zone = p.TimeZone
			}
			if _, err = time.LoadLocation(zone); err != nil {
				return RemotePage{}, errors.New("provider zone invalid")
			}
			page.Entries = append(page.Entries, RemoteEntry{id, row.ETag, calendar, matter, row.Title, row.Description, zone, start, end, row.AllDay})
		}
	}
	return page, nil
}
func (p *Provider) MatterUpdate(ctx context.Context, id, title, etag, token string) (string, error) {
	if id == "" || len(id) > 200 {
		return "", errors.New("invalid external id")
	}
	path := p.Base + p.resourcePath("matter")
	if p.ID == "clio-manage" {
		path = p.Base + "/matters/" + url.PathEscape(id) + ".json"
	} else {
		path = path + "/" + url.PathEscape(id)
	}
	field := "title"
	if p.ID == "clio-manage" {
		field = "description"
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]string{field: title}})
	resp, err := p.request(ctx, "PATCH", path, token, etag, raw)
	if err != nil {
		return "uncertain", nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == 412 {
		return "conflict", nil
	}
	if resp.StatusCode != 200 {
		return "uncertain", nil
	}
	return "completed", nil
}
