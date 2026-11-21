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
	"time"

	"github.com/QuantumNous/new-api/common"
)

type supplyLoginSession struct {
	base   string
	client *http.Client
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
	if res.StatusCode != 200 || common.DecodeJson(io.LimitReader(res.Body, 1<<20), &envelope) != nil || !envelope.Success {
		return errors.New("Downstream sign-in failed")
	}
	return nil
}

// VerifySupplyAccount authenticates once against the customer's own dashboard.
// Its password, cookie jar and bearer token never enter a model or a log.
func VerifySupplyAccount(ctx context.Context, base, username, password string) (int, error) {
	if !SupplyURL(base) || username == "" || password == "" {
		return 0, errors.New("Invalid parameters")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var status struct {
		Encryption      bool  `json:"password_login_encryption_enabled"`
		Interactive     bool  `json:"turnstile_check"`
		PasswordEnabled *bool `json:"password_login_enabled"`
	}
	session := supplyLoginSession{base: base, client: client}

	if err := session.request(ctx, "GET", "/api/status", nil, "", 0, &status); err != nil {
		return 0, err
	}
	if status.Interactive || status.PasswordEnabled != nil && !*status.PasswordEnabled {
		return 0, errors.New("Downstream requires interactive sign-in")
	}
	payload := map[string]string{"username": username}
	if status.Encryption {
		var key struct {
			KID       string
			PublicKey string `json:"public_key"`
		}
		if err := session.request(ctx, "GET", "/api/user/login/encryption-key", nil, "", 0, &key); err != nil {
			return 0, err
		}
		block, _ := pem.Decode([]byte(key.PublicKey))
		if block == nil || key.KID == "" {
			return 0, errors.New("Downstream sign-in failed")
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		pub, ok := parsed.(*rsa.PublicKey)
		if err != nil || !ok || pub.N.BitLen() < 2048 {
			return 0, errors.New("Downstream sign-in failed")
		}
		// Match the dashboard's v2 envelope, including long Unicode passwords.
		secret := make([]byte, 32)
		rand.Read(secret)
		wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, secret, []byte("password-v2"))
		if err != nil {
			return 0, errors.New("Downstream sign-in failed")
		}
		c, _ := aes.NewCipher(secret)
		gcm, err := cipher.NewGCM(c)
		if err != nil {
			return 0, err
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
	}
	if err := session.request(ctx, "POST", "/api/user/login", payload, "", 0, &login); err != nil {
		return 0, err
	}
	uid := login.User.ID
	if uid == 0 {
		uid = login.ID
	}
	if uid <= 0 {
		return 0, errors.New("Downstream requires interactive sign-in")
	}
	if login.AccessToken != "" {
		defer session.request(context.WithoutCancel(ctx), "POST", "/api/user/auth/logout", nil, login.AccessToken, uid, nil)
	} else {
		defer session.request(context.WithoutCancel(ctx), "GET", "/api/user/logout", nil, "", uid, nil)
	}
	var self struct {
		ID       int
		Username string
	}
	if err := session.request(ctx, "GET", "/api/user/self", nil, login.AccessToken, uid, &self); err != nil {
		return 0, err
	}
	if self.ID != uid || self.Username == "" {
		return 0, errors.New("Downstream sign-in failed")
	}
	return uid, nil
}

func DirectSupplyAddress(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !SupplyURL(strings.TrimSpace(raw)) || len(raw) > 512 {
		return "", errors.New("Invalid parameters")
	}
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
	}
	return strings.TrimRight(u.String(), "/"), nil
}
