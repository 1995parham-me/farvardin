package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Errors of the Web Push layer.
var (
	ErrBadKey      = errors.New("push: VAPID key is not a P-256 EC private key")
	ErrBadEndpoint = errors.New("push: push resource is not an https URL")
	ErrBadSubKey   = errors.New("push: subscription public key is not a P-256 point")
)

// Keys is the server's VAPID key pair (RFC 8292).
type Keys struct {
	private *ecdsa.PrivateKey
	// Public is the uncompressed P-256 point, base64url without padding,
	// as published in the transports property.
	Public  string
	Subject string
}

// LoadKeys reads a PEM private key (PKCS#8 or SEC 1, as py_vapid and
// Xandikos write it), or creates one with mode 0600 when path is missing.
func LoadKeys(path, subject string) (*Keys, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path comes from the operator's config
	if errors.Is(err, os.ErrNotExist) {
		data, err = generateKey(path)
	}

	if err != nil {
		return nil, fmt.Errorf("push: reading VAPID key: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, ErrBadKey
	}

	var key *ecdsa.PrivateKey

	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, ErrBadKey
		}

		key = ec
	} else if key, err = x509.ParseECPrivateKey(block.Bytes); err != nil {
		return nil, ErrBadKey
	}

	if key.Curve != elliptic.P256() {
		return nil, ErrBadKey
	}

	pub, err := key.PublicKey.ECDH()
	if err != nil {
		return nil, ErrBadKey
	}

	return &Keys{private: key, Public: b64(pub.Bytes()), Subject: subject}, nil
}

func generateKey(path string) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by the caller
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err //nolint:wrapcheck // wrapped by the caller
	}

	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Headers: nil, Bytes: der})

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err //nolint:wrapcheck // wrapped by the caller
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err //nolint:wrapcheck // wrapped by the caller
	}

	return data, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// unb64 accepts base64url with or without padding.
func unb64(s string) ([]byte, error) {
	for len(s)%4 != 0 {
		s += "="
	}

	return base64.URLEncoding.DecodeString(s) //nolint:wrapcheck // callers add context
}

// vapidHeader builds "vapid t=<JWT>, k=<public key>" for the push
// service that owns endpoint.
func (k *Keys) vapidHeader(endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", ErrBadEndpoint
	}

	header := b64([]byte(`{"typ":"JWT","alg":"ES256"}`))

	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(12 * time.Hour).Unix(),
		"sub": k.Subject,
	})
	if err != nil {
		return "", fmt.Errorf("push: encoding claims: %w", err)
	}

	signing := header + "." + b64(claims)
	digest := sha256.Sum256([]byte(signing))

	r, s, err := ecdsa.Sign(rand.Reader, k.private, digest[:])
	if err != nil {
		return "", fmt.Errorf("push: signing: %w", err)
	}

	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	return "vapid t=" + signing + "." + b64(sig) + ", k=" + k.Public, nil
}

// recordSize is the RFC 8188 record size written in the header.
const recordSize = 4096

// encrypt seals payload for a subscriber with RFC 8291 (aes128gcm),
// with a fresh ephemeral key and salt.
func encrypt(payload, uaPublic, authSecret []byte) ([]byte, error) {
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("push: ephemeral key: %w", err)
	}

	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("push: salt: %w", err)
	}

	return encryptWith(payload, uaPublic, authSecret, as, salt)
}

func encryptWith(payload, uaPublic, authSecret []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, ErrBadSubKey
	}

	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, fmt.Errorf("push: ECDH: %w", err)
	}

	asPublic := as.PublicKey().Bytes()

	// RFC 8291 section 3.4: IKM from the ECDH secret and the auth secret.
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)

	ikm, err := hkdf.Key(sha256.New, shared, authSecret, string(keyInfo), 32)
	if err != nil {
		return nil, fmt.Errorf("push: HKDF: %w", err)
	}

	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, fmt.Errorf("push: HKDF: %w", err)
	}

	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, fmt.Errorf("push: HKDF: %w", err)
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("push: AES: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("push: GCM: %w", err)
	}

	// One record: the payload and the 0x02 "last record" delimiter.
	plain := append(append([]byte(nil), payload...), 0x02)

	var out bytes.Buffer

	out.Write(salt)
	_ = binary.Write(&out, binary.BigEndian, uint32(recordSize))
	out.WriteByte(byte(len(asPublic))) //nolint:gosec // an uncompressed P-256 point is always 65 bytes
	out.Write(asPublic)
	out.Write(gcm.Seal(nil, nonce, plain, nil))

	return out.Bytes(), nil
}

// send delivers one encrypted message and returns the push service's
// status code.
func (k *Keys) send(ctx context.Context, client *http.Client, sub Subscription, payload []byte, topic string) (int, error) {
	p256dh, err := unb64(sub.P256DH)
	if err != nil {
		return 0, fmt.Errorf("push: p256dh: %w", err)
	}

	auth, err := unb64(sub.AuthSecret)
	if err != nil {
		return 0, fmt.Errorf("push: auth secret: %w", err)
	}

	body, err := encrypt(payload, p256dh, auth)
	if err != nil {
		return 0, err
	}

	authz, err := k.vapidHeader(sub.PushResource, time.Now())
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.PushResource, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("push: request: %w", err)
	}

	req.Header.Set("Authorization", authz)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(time.Hour/time.Second)))
	req.Header.Set("Urgency", "normal")
	// RFC 8030 Topic: a newer message for the collection replaces one the
	// device has not picked up yet.
	if topic != "" && len(topic) <= 32 {
		req.Header.Set("Topic", topic)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("push: delivering: %w", err)
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	return resp.StatusCode, nil
}
