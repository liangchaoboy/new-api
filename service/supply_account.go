package service

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

type supplyAccountProfile struct {
	ID          int    `json:"id"`
	Role        int    `json:"role"`
	Username    string `json:"username"`
	Permissions struct {
		Channels map[string]map[string]bool `json:"admin_permissions"`
	} `json:"permissions"`
}
type supplyLoginSession struct {
	user    supplyAccountProfile
	bearer  string
	expires int64
	base    string
	client  *http.Client
}

func (s supplyLoginSession) request(ctx context.Context, method, path string, body any, bearer string, uid int, out any) error {
	var data []byte
	if body != nil {
		var err error
		data, err = common.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("Invalid parameters")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", req.URL.Scheme+"://"+req.URL.Host)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if uid > 0 {
		req.Header.Set("New-Api-User", strconv.Itoa(uid))
	}
	res, err := s.client.Do(req)
	if err != nil {
		return errors.New("Target unavailable")
	}
	defer res.Body.Close()
	var envelope struct {
		Success bool
		Data    any
	}
	envelope.Data = out
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
		return errCustomerEndpointMissing
	}
	if res.StatusCode == http.StatusUnauthorized {
		return errCustomerSessionExpired
	}
	if res.StatusCode == http.StatusForbidden {
		return ErrCustomerDenied
	}
	if res.StatusCode != 200 || common.DecodeJson(io.LimitReader(res.Body, 4<<20), &envelope) != nil {
		return errors.New("Target unavailable")
	}
	if !envelope.Success {
		return errCustomerRejected
	}
	return nil
}

func loginSupplyAccount(ctx context.Context, base, username, password string) (*supplyLoginSession, error) {
	if !SupplyURL(base) || username == "" || password == "" {
		return nil, errors.New("Invalid parameters")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var status struct {
		Encryption      bool  `json:"password_login_encryption_enabled"`
		Interactive     bool  `json:"turnstile_check"`
		PasswordEnabled *bool `json:"password_login_enabled"`
	}
	session := supplyLoginSession{base: base, client: client}

	if err := session.request(ctx, "GET", "/api/status", nil, "", 0, &status); err != nil {
		return nil, err
	}
	if status.Interactive || status.PasswordEnabled != nil && !*status.PasswordEnabled {
		return nil, errors.New("Downstream requires interactive sign-in")
	}
	payload := map[string]string{"username": username}
	if status.Encryption {
		var key struct {
			KID       string
			PublicKey string `json:"public_key"`
		}
		if err := session.request(ctx, "GET", "/api/user/login/encryption-key", nil, "", 0, &key); err != nil {
			return nil, err
		}
		block, _ := pem.Decode([]byte(key.PublicKey))
		if block == nil || key.KID == "" {
			return nil, errors.New("Downstream sign-in failed")
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		pub, ok := parsed.(*rsa.PublicKey)
		if err != nil || !ok || pub.N.BitLen() < 2048 {
			return nil, errors.New("Downstream sign-in failed")
		}
		// Match the dashboard's v2 envelope, including long Unicode passwords.
		secret := make([]byte, 32)
		rand.Read(secret)
		wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, secret, []byte("password-v2"))
		if err != nil {
			return nil, errors.New("Downstream sign-in failed")
		}
		c, _ := aes.NewCipher(secret)
		gcm, err := cipher.NewGCM(c)
		if err != nil {
			return nil, err
		}
		nonce := make([]byte, gcm.NonceSize())
		rand.Read(nonce)
		enc := base64.StdEncoding.EncodeToString
		payload["password_encrypted"] = strings.Join([]string{"v2", enc(wrapped), enc(nonce), enc(gcm.Seal(nil, nonce, []byte(password), []byte("password-v2:"+key.KID)))}, ".")
		payload["encryption_key_id"] = key.KID
	} else {
		payload["password"] = password
	}
	var login struct {
		ID          int
		User        struct{ ID int }
		AccessToken string `json:"access_token"`
		Expires     int64  `json:"access_expires_at"`
	}
	if err := session.request(ctx, "POST", "/api/user/login", payload, "", 0, &login); err != nil {
		return nil, err
	}
	uid := login.User.ID
	if uid == 0 {
		uid = login.ID
	}
	if uid <= 0 {
		return nil, errors.New("Downstream requires interactive sign-in")
	}
	session.bearer = login.AccessToken
	session.user.ID = uid
	session.expires = login.Expires
	if session.expires == 0 {
		session.expires = time.Now().Add(10 * time.Minute).Unix()
	}
	var self supplyAccountProfile
	if err := session.api(ctx, "GET", "/api/user/self", nil, &self); err != nil {
		session.close(ctx)
		return nil, err
	}
	if self.ID != uid || self.Username == "" {
		session.close(ctx)
		return nil, errors.New("Downstream sign-in failed")
	}
	session.user = self
	return &session, nil
}

func (s *supplyLoginSession) close(ctx context.Context) {
	method, path := "GET", "/api/user/logout"
	if s.bearer != "" {
		method, path = "POST", "/api/user/auth/logout"
	}
	_ = s.api(context.WithoutCancel(ctx), method, path, nil, nil)
}
func (s *supplyLoginSession) api(ctx context.Context, method, path string, body, out any) error {
	return s.request(ctx, method, path, body, s.bearer, s.user.ID, out)
}

func VerifySupplyAccount(ctx context.Context, base, username, password string) (int, error) {
	s, err := loginSupplyAccount(ctx, base, username, password)
	if err != nil {
		return 0, err
	}
	defer s.close(ctx)
	return s.user.ID, nil
}

var errCustomerRejected = errors.New("Target rejected the request")
var errCustomerEndpointMissing = errors.New("Customer endpoint unavailable")
var errCustomerSessionExpired = errors.New("Downstream sign-in failed")
var ErrCustomerDenied = errors.New("Customer account lacks the required permissions")

type customerSessionCache struct {
	mu         sync.Mutex
	credential string
	session    *supplyLoginSession
	retryAfter time.Time
}

var customerSessions sync.Map

func customerAccountAAD(t model.SupplyTarget) string {
	return "customer-account/" + t.ID + "/" + t.URL + "/" + t.LoginName
}

// A verification checks the account that A actually issued. Self logs are
// intentionally excluded: those count the caller's consumption across channels.
func CheckCustomerAccount(ctx context.Context, t model.SupplyTarget, password string) (int, string, string, error) {
	s, err := loginSupplyAccount(ctx, t.URL, t.LoginName, password)
	if err != nil {
		return 0, "", "", err
	}
	defer s.close(ctx)
	var page struct {
		Items *[]struct{} `json:"items"`
		Total *int        `json:"total"`
	}
	access, usage := "", ""
	if err := s.api(ctx, "GET", "/api/channel/search?keyword=supply-capability-check&page_size=1", nil, &page); err == nil && page.Items != nil && page.Total != nil {
		access = model.SupplyAPIOfficial
		if p := s.user.Permissions.Channels["channel"]; p != nil && s.user.Role != common.RoleRootUser && (!p["write"] || !p["sensitive_write"] || !p["operate"]) {
			access = ""
		}
	}
	day := common.GetTimestamp() / 86400 * 86400
	var daily *[]customerDailyRow
	if err := s.api(ctx, "GET", customerDailyPath(day), nil, &daily); err == nil && daily != nil {
		usage = "channel_daily"
	} else {
		page.Items = nil
		page.Total = nil
		if err := s.api(ctx, "GET", "/api/log/?type=2&page_size=1&start_timestamp="+strconv.FormatInt(day, 10), nil, &page); err == nil && page.Items != nil && page.Total != nil {
			usage = "channel_logs"
		}
	}
	return s.user.ID, access, usage, nil
}

func withCustomerSession(ctx context.Context, t model.SupplyTarget, work func(*supplyLoginSession) error) error {
	entry, _ := customerSessions.LoadOrStore(t.ID, &customerSessionCache{})
	cache := entry.(*customerSessionCache)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.credential != t.Secret {
		if cache.session != nil {
			cache.session.close(ctx)
		}
		cache.session = nil
		cache.credential = t.Secret
		cache.retryAfter = time.Time{}
	}
	if cache.session != nil && cache.session.expires <= time.Now().Add(10*time.Second).Unix() {
		cache.session.close(ctx)
		cache.session = nil
	}
	if cache.session == nil {
		if time.Now().Before(cache.retryAfter) {
			return errCustomerSessionExpired
		}
		password, err := openSupplySecret(customerAccountAAD(t), t.Secret)
		if err != nil {
			return errors.New("Downstream sign-in failed")
		}
		session, err := loginSupplyAccount(ctx, t.URL, t.LoginName, password)
		if err != nil {
			cache.retryAfter = time.Now().Add(time.Minute)
			return err
		}
		if session.user.ID != t.Issuer {
			session.close(ctx)
			cache.retryAfter = time.Now().Add(time.Minute)
			return model.ErrSupplyConflict
		}
		cache.session = session
	}
	err := work(cache.session)
	if errors.Is(err, errCustomerSessionExpired) {
		cache.session = nil
		cache.retryAfter = time.Now().Add(time.Minute)
	}
	return err
}

func DirectSupplyAddress(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !SupplyURL(strings.TrimSpace(raw)) || len(raw) > 512 {
		return "", errors.New("Invalid parameters")
	}
	if strings.TrimRight(u.Path, "/") == "/sign-in" {
		u.Path = ""
		u.RawPath = ""
	}
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func SealCustomerPassword(target model.SupplyTarget, password string) (string, error) {
	return SealSupplySecret(customerAccountAAD(target), password)
}
